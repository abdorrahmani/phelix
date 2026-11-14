package hostsecurity

import (
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/securityscore"
)

type Source struct {
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	RetrievedAt time.Time `json:"retrieved_at"`
	Modified    string    `json:"modified,omitempty"`
	Digest      string    `json:"digest"`
	Stale       bool      `json:"stale"`
	Error       string    `json:"error,omitempty"`
}

type Finding struct {
	ID            string                     `json:"id"`
	PackageKey    string                     `json:"package_key"`
	Package       string                     `json:"package"`
	Version       string                     `json:"version"`
	CVE           string                     `json:"cve"`
	AdvisoryID    string                     `json:"advisory_id"`
	Ecosystem     string                     `json:"ecosystem"`
	FixedVersion  string                     `json:"fixed_version,omitempty"`
	AffectedRange string                     `json:"affected_range"`
	State         string                     `json:"state"`
	Reason        string                     `json:"reason"`
	Severity      string                     `json:"severity"`
	Sources       []Source                   `json:"sources"`
	References    []string                   `json:"references"`
	Published     string                     `json:"published,omitempty"`
	Modified      string                     `json:"modified,omitempty"`
	Risk          securityscore.Risk         `json:"risk"`
	Priority      securityscore.Contribution `json:"priority"`
}

// defaultEnrichmentBudget bounds the whole enrichment phase of one scan so
// unreachable enrichment providers can never consume the scan's own time
// budget.
const defaultEnrichmentBudget = 90 * time.Second

type Intelligence struct {
	CacheDir string
	HTTP     *http.Client
	Offline  bool
	URLs     map[string]string
	Now      func() time.Time
	// EnrichmentBudget bounds the total enrichment phase (per-CVE
	// providers, KEV, EPSS) of one scan, derived from the scan context.
	// Zero uses the default. When it expires enrichment stops cleanly:
	// the distribution findings stand as they are and the scan records a
	// warning instead of failing.
	EnrichmentBudget time.Duration
}

func (i *Intelligence) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now().UTC()
}

func (i *Intelligence) fetch(ctx context.Context, name, address string) ([]byte, Source, error) {
	if override := i.URLs[name]; override != "" {
		address = override
	}
	source := Source{Name: name, URL: address}
	sum := sha256.Sum256([]byte(address))
	path := filepath.Join(i.CacheDir, hex.EncodeToString(sum[:])+".json")
	var cached struct {
		Source Source
		Body   []byte
	}
	raw, readErr := os.ReadFile(path)
	if readErr == nil {
		if err := json.Unmarshal(raw, &cached); err != nil {
			readErr = err
		}
	}
	if readErr == nil && cached.Source.Digest != "" {
		digest := sha256.Sum256(cached.Body)
		if cached.Source.Digest != hex.EncodeToString(digest[:]) {
			readErr = errors.New("cache digest mismatch")
		}
	}
	if readErr == nil && (i.Offline || i.now().Sub(cached.Source.RetrievedAt) < 6*time.Hour) {
		cached.Source.Stale = i.now().Sub(cached.Source.RetrievedAt) > 24*time.Hour
		return cached.Body, cached.Source, nil
	}
	var fetchErr error
	if !i.Offline {
		client := i.HTTP
		if client == nil {
			client = &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 3 || req.URL.Scheme != "https" {
					return errors.New("unsafe advisory redirect")
				}
				return nil
			}}
		}
		for attempt := 0; attempt < 3; attempt++ {
			requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, address, nil)
			if err != nil {
				cancel()
				return nil, source, err
			}
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "Phelix-Security/1")
			resp, err := client.Do(req)
			if err != nil {
				cancel()
				fetchErr = errors.New("advisory request failed")
			} else {
				if resp.StatusCode == http.StatusOK {
					body, e := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
					resp.Body.Close()
					cancel()
					if e != nil || len(body) == 64<<20 {
						return nil, source, errors.New("advisory response incomplete or too large")
					}
					digest := sha256.Sum256(body)
					source.RetrievedAt = i.now()
					source.Modified = resp.Header.Get("Last-Modified")
					source.Digest = hex.EncodeToString(digest[:])
					cached.Source = source
					cached.Body = body
					data, e := json.Marshal(cached)
					if e != nil {
						return nil, source, e
					}
					if e = os.MkdirAll(i.CacheDir, 0700); e != nil {
						return nil, source, e
					}
					f, e := os.CreateTemp(i.CacheDir, "advisory-*")
					if e != nil {
						return nil, source, e
					}
					tmp := f.Name()
					_, e = f.Write(data)
					if e == nil {
						e = f.Sync()
					}
					closeErr := f.Close()
					if e == nil {
						e = closeErr
					}
					if e == nil {
						e = os.Rename(tmp, path)
					}
					if e != nil {
						os.Remove(tmp)
						return nil, source, e
					}
					return body, source, nil
				}
				status := resp.StatusCode
				retryAfter := resp.Header.Get("Retry-After")
				resp.Body.Close()
				cancel()
				fetchErr = fmt.Errorf("advisory source returned HTTP %d", status)
				if status != 429 && status < 500 {
					break
				}
				if seconds, e := strconv.Atoi(retryAfter); e == nil && seconds > 0 {
					if e = waitContext(ctx, time.Duration(min(seconds, 15))*time.Second); e != nil {
						return nil, source, e
					}
				}
			}
			if attempt < 2 {
				if err := waitContext(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
					return nil, source, err
				}
			}
		}
	} else {
		fetchErr = errors.New("offline mode and no cached intelligence")
	}
	if readErr == nil {
		cached.Source.Stale = true
		cached.Source.Error = fetchErr.Error()
		return cached.Body, cached.Source, nil
	}
	source.Error = fetchErr.Error()
	return nil, source, fetchErr
}

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var cvePattern = regexp.MustCompile(`CVE-[0-9]{4}-[0-9]{4,}`)

func CVEs(text string) []string {
	ids := cvePattern.FindAllString(strings.ToUpper(text), -1)
	sort.Strings(ids)
	out := []string{}
	for _, id := range ids {
		if len(out) == 0 || out[len(out)-1] != id {
			out = append(out, id)
		}
	}
	return out
}

func finding(h Host, p Package, cve, advisory, fixed, severity, state, reason string, source Source) Finding {
	stable := sha256.Sum256([]byte(p.Name + ":" + p.Architecture + ":" + cve))
	f := Finding{ID: hex.EncodeToString(stable[:16]), PackageKey: p.Key, Package: p.Name, Version: p.Version, CVE: cve, AdvisoryID: advisory, Ecosystem: h.ID + ":" + h.Version, FixedVersion: fixed, Severity: strings.ToLower(severity), State: state, Reason: reason, Sources: []Source{source}, References: []string{source.URL}}
	if f.Severity == "" {
		f.Severity = "unknown"
	}
	if fixed != "" {
		f.AffectedRange = "< " + fixed
	} else {
		f.AffectedRange = "distribution advisory status"
	}
	f.Risk = securityscore.Risk{ID: cve, Severity: f.Severity, Fixed: fixed != "", Confidence: state}
	return f
}

// Unknown-package warning bounds: a loaded coverage set that claims the
// release but omits this many installed packages records an informational
// warning. The count never gates match completeness: advisory sources only
// list packages with recorded issues, so a large unknown count is expected
// on a quiet system.
const (
	coverageUnknownRatioPercent  = 5
	coverageUnknownAbsoluteBound = 50
)

// coverageDegraded reports whether the unknown-package count crosses the
// informational warning bounds: over the absolute bound, or over the ratio
// of the installed packages. The name keeps the historical threshold
// vocabulary; the count only warns, it never degrades coverage.
func coverageDegraded(total, unknown int) bool {
	if unknown <= 0 {
		return false
	}
	return unknown > coverageUnknownAbsoluteBound || unknown*100 > total*coverageUnknownRatioPercent
}

func (i *Intelligence) Match(ctx context.Context, system System, h Host, packages []Package) ([]Finding, []Source, bool, int, error) {
	var findings []Finding
	var sources []Source
	var unknown int
	var err error
	switch h.ID {
	case "debian":
		findings, sources, unknown, err = i.debian(ctx, system, h, packages)
	case "ubuntu":
		findings, sources, unknown, err = i.ubuntu(ctx, system, h, packages)
	case "alpine":
		findings, sources, unknown, err = i.alpine(ctx, system, h, packages)
	case "arch":
		findings, sources, unknown, err = i.arch(ctx, system, h, packages)
	default:
		findings, sources, unknown, err = i.rpmAdvisories(ctx, system, h, packages)
	}
	findings = deduplicate(findings)
	budget := i.EnrichmentBudget
	if budget <= 0 {
		budget = defaultEnrichmentBudget
	}
	enrichCtx, cancelEnrichment := context.WithTimeout(ctx, budget)
	sources = append(sources, i.enrich(enrichCtx, findings)...)
	budgetExhausted := errors.Is(enrichCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancelEnrichment()
	if budgetExhausted {
		// Enrichment is additive risk context: when its own budget runs
		// out the distribution findings stand as they are and the scan
		// records the shortfall instead of failing.
		sources = append(sources, Source{Name: "enrichment", Error: "enrichment budget exceeded"})
	}
	complete := err == nil
	for _, f := range findings {
		if f.State != "confirmed" {
			complete = false
		}
	}
	if err == nil && coverageDegraded(len(packages), unknown) {
		// Informational only: advisory sources list just the packages
		// with recorded issues, so absent packages are expected and the
		// count never gates match completeness.
		sources = append(sources, Source{Name: "coverage", Error: fmt.Sprintf("%d of %d installed packages are absent from the loaded distribution coverage; the count is informational because advisory sources only list packages with recorded issues", unknown, len(packages))})
	}
	for n := range findings {
		findings[n].Priority = securityscore.Prioritize(findings[n].Risk)
	}
	sort.Slice(findings, func(a, b int) bool {
		if findings[a].Priority.Points != findings[b].Priority.Points {
			return findings[a].Priority.Points > findings[b].Priority.Points
		}
		return findings[a].ID < findings[b].ID
	})
	return findings, sources, complete, unknown, err
}

func deduplicate(findings []Finding) []Finding {
	out := []Finding{}
	indices := map[string]int{}
	for _, f := range findings {
		key := f.PackageKey + ":" + f.CVE
		if idx, ok := indices[key]; ok {
			out[idx].Sources = append(out[idx].Sources, f.Sources...)
			out[idx].References = append(out[idx].References, f.References...)
			if f.State != "confirmed" {
				out[idx].State = f.State
				out[idx].Risk.Confidence = f.State
			}
		} else {
			indices[key] = len(out)
			out = append(out, f)
		}
	}
	return out
}

func decodeObject(raw []byte, compressed bool, visit func(string, json.RawMessage) error) error {
	var reader io.Reader = bytes.NewReader(raw)
	if compressed {
		reader = bzip2.NewReader(reader)
	}
	d := json.NewDecoder(io.LimitReader(reader, 512<<20))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid advisory object")
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
		if err = visit(key.(string), value); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	return u.String()
}
