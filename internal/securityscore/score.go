package securityscore

import (
	"math"
	"sort"
	"time"
)

const Version = "risk-v1"

type Risk struct {
	ID               string   `json:"id"`
	Severity         string   `json:"severity"`
	CVSS             *float64 `json:"cvss,omitempty"`
	EPSS             *float64 `json:"epss,omitempty"`
	KnownExploited   *bool    `json:"known_exploited,omitempty"`
	ExploitAvailable *bool    `json:"exploit_available,omitempty"`
	NetworkExposed   *bool    `json:"network_exposed,omitempty"`
	CriticalService  *bool    `json:"critical_service,omitempty"`
	ImportantPackage *bool    `json:"important_package,omitempty"`
	Fixed            bool     `json:"fixed"`
	Confidence       string   `json:"confidence"`
}

type Contribution struct {
	ID       string   `json:"id"`
	Points   int      `json:"points"`
	Priority string   `json:"priority"`
	Reasons  []string `json:"reasons"`
}

type Score struct {
	Version   string         `json:"version"`
	Value     *int           `json:"value"`
	Status    string         `json:"status"`
	At        time.Time      `json:"at"`
	Breakdown []Contribution `json:"breakdown"`
	Reasons   []string       `json:"reasons"`
}

func Prioritize(r Risk) Contribution {
	weights := map[string]int{"critical": 20, "high": 10, "medium": 4, "low": 1, "informational": 0, "unknown": 4}
	base, ok := weights[r.Severity]
	if !ok {
		base = 4
	}
	c := Contribution{ID: r.ID, Reasons: []string{"severity: " + r.Severity}}
	priority := r.Severity
	if !ok {
		priority = "unknown"
	}
	multiplier := 1.0
	if r.CVSS != nil && *r.CVSS >= 0 && *r.CVSS <= 10 {
		multiplier += *r.CVSS / 100
		c.Reasons = append(c.Reasons, "CVSS adds up to 10% within the severity band")
	}
	for _, factor := range []struct {
		value  *bool
		weight float64
		reason string
	}{{r.KnownExploited, 1, "known exploitation"}, {r.ExploitAvailable, .25, "public exploit available"}, {r.NetworkExposed, .25, "network exposed"}, {r.CriticalService, .25, "critical service"}, {r.ImportantPackage, .1, "important package"}} {
		if factor.value == nil {
			c.Reasons = append(c.Reasons, factor.reason+": unknown")
		} else if *factor.value {
			multiplier += factor.weight
			c.Reasons = append(c.Reasons, factor.reason)
		}
	}
	if r.EPSS == nil {
		c.Reasons = append(c.Reasons, "EPSS: unknown")
	} else if *r.EPSS >= 0 && *r.EPSS <= 1 {
		multiplier += .5 * (*r.EPSS)
		c.Reasons = append(c.Reasons, "EPSS increases priority by predicted exploitation probability")
	}
	if r.Fixed {
		multiplier += .1
		c.Reasons = append(c.Reasons, "fixed version available")
	}
	c.Points = int(math.Ceil(float64(base) * multiplier))
	if r.Confidence != "confirmed" {
		c.Points += 2
		c.Reasons = append(c.Reasons, "unverified finding adds uncertainty penalty")
	}
	if r.KnownExploited != nil && *r.KnownExploited {
		priority = "critical"
	} else if priority != "unknown" && priority != "informational" {
		switch {
		case c.Points >= 20:
			priority = "critical"
		case c.Points >= 10:
			priority = "high"
		case c.Points >= 4:
			priority = "medium"
		default:
			priority = "low"
		}
	}
	c.Priority = priority
	return c
}

func Calculate(risks []Risk, complete, fresh bool, scannedAt, now time.Time) Score {
	s := Score{Version: Version, At: now, Status: "unknown", Breakdown: []Contribution{}, Reasons: []string{}}
	byID := map[string]Contribution{}
	for _, r := range risks {
		c := Prioritize(r)
		if old, ok := byID[c.ID]; !ok || c.Points > old.Points {
			byID[c.ID] = c
		}
	}
	keys := make([]string, 0, len(byID))
	for id := range byID {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	penalty := 0
	for _, id := range keys {
		c := byID[id]
		penalty += c.Points
		s.Breakdown = append(s.Breakdown, c)
	}
	if !complete {
		s.Reasons = append(s.Reasons, "incomplete intelligence or inventory coverage")
	}
	if scannedAt.IsZero() {
		s.Reasons = append(s.Reasons, "no successful scan")
	}
	if !fresh {
		s.Reasons = append(s.Reasons, "stale intelligence adds 10 points")
		penalty += 10
	}
	if !scannedAt.IsZero() {
		age := max(0, int(now.Sub(scannedAt).Hours()/24)-1)
		if age > 0 {
			points := min(20, age*2)
			penalty += points
			s.Breakdown = append(s.Breakdown, Contribution{ID: "scan-age", Points: points, Priority: "informational", Reasons: []string{"2 points per full day after the first 24 hours, capped at 20"}})
		}
	}
	if !complete || scannedAt.IsZero() {
		return s
	}
	value := max(0, 100-penalty)
	s.Value = &value
	s.Status = "needs_attention"
	if value >= 90 && len(risks) == 0 && fresh {
		s.Status = "healthy"
	}
	if value < 50 {
		s.Status = "high_risk"
	}
	return s
}
