package project

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"go.yaml.in/yaml/v3"
)

// SecurityConfig is the phelix.yaml shape of application security
// scanning. An app without the section never schedules anything; manual
// scans remain available regardless of configuration.
type SecurityConfig struct {
	Vulnerability *VulnerabilityConfig `yaml:"vulnerability,omitempty"`
}

// VulnerabilityConfig configures application dependency vulnerability
// scanning. enabled defaults to false: scheduled scanning is opt-in.
type VulnerabilityConfig struct {
	// Enabled turns scheduled scans on for this app. Manual scans work
	// with or without it.
	Enabled bool `yaml:"enabled"`
	// ScanInterval is the cadence of scheduled scans, anchored to scan
	// completion. Defaults to 24h; the minimum is 15m.
	ScanInterval ScanInterval `yaml:"scan_interval,omitempty"`
	// FailThreshold is the severity a finding must reach for a scan to
	// report the dedicated security findings exit code. Defaults to high.
	FailThreshold ScanSeverity `yaml:"fail_threshold,omitempty"`
	// Reporting opts this application in to transmitting security data
	// (scan results, inventory, graphs) to the Phelix backend over the
	// monitoring stream. It defaults to false: dependency names and
	// versions are private, and nothing leaves the host until the user
	// says so. Remote security operations against this app are refused
	// with SECURITY_REPORTING_DISABLED while reporting is off.
	Reporting bool `yaml:"reporting,omitempty"`
}

// ScanInterval is a duration written in Go duration syntax ("24h",
// "90m"). yaml.v3 decodes into a plain string so validation can name the
// exact yaml path instead of a malformed-file error.
type ScanInterval string

// InvalidScanInterval is the sentinel stored for an unparseable entry.
const InvalidScanInterval ScanInterval = "<invalid>"

// ScanIntervalDefault is the cadence when the key is absent.
const ScanIntervalDefault = 24 * time.Hour

// ScanIntervalMinimum is the fastest cadence the scheduler accepts.
const ScanIntervalMinimum = 15 * time.Minute

// UnmarshalYAML implements yaml.Unmarshaler. It never fails; unparseable
// entries become InvalidScanInterval, reported by validate() with the
// yaml path.
func (i *ScanInterval) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		*i = InvalidScanInterval
		return nil
	}
	*i = ScanInterval(strings.TrimSpace(node.Value))
	return nil
}

// MarshalYAML implements yaml.Marshaler, writing the plain string so a
// saved file round-trips.
func (i ScanInterval) MarshalYAML() (any, error) { return string(i), nil }

// Duration parses the interval. An empty value yields the default.
func (i ScanInterval) Duration() (time.Duration, error) {
	if i == "" {
		return ScanIntervalDefault, nil
	}
	return time.ParseDuration(string(i))
}

// ScanSeverity is one of low, medium, high, critical. Unknown advisory
// severities are a separate value in reports and never match a threshold.
type ScanSeverity string

// InvalidScanSeverity is the sentinel stored for an unparseable entry.
const InvalidScanSeverity ScanSeverity = "<invalid>"

// ScanSeverityDefault is the threshold when the key is absent.
const ScanSeverityDefault ScanSeverity = "high"

// UnmarshalYAML implements yaml.Unmarshaler. It never fails; unparseable
// entries become InvalidScanSeverity, reported by validate().
func (s *ScanSeverity) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		*s = InvalidScanSeverity
		return nil
	}
	*s = ScanSeverity(strings.TrimSpace(node.Value))
	return nil
}

// MarshalYAML implements yaml.Marshaler, writing the plain string so a
// saved file round-trips.
func (s ScanSeverity) MarshalYAML() (any, error) { return string(s), nil }

// Valid reports whether the severity is one of the supported values.
func (s ScanSeverity) Valid() bool {
	switch s {
	case "low", "medium", "high", "critical":
		return true
	default:
		return false
	}
}

// Validate is the exported entry to the security section's validation,
// the same rules project.Load enforces.
func (s *SecurityConfig) Validate() error { return s.validate() }

// ResolvedScanInterval returns the effective cadence, applying the
// default when the key is absent. Validation guarantees a present value
// parses and meets the minimum.
func (v *VulnerabilityConfig) ResolvedScanInterval() (time.Duration, error) {
	return v.ScanInterval.Duration()
}

// ResolvedFailThreshold returns the effective severity threshold,
// applying the default when the key is absent.
func (v *VulnerabilityConfig) ResolvedFailThreshold() ScanSeverity {
	if v.FailThreshold == "" {
		return ScanSeverityDefault
	}
	return v.FailThreshold
}

// ResolvedReporting returns whether the app consents to remote security
// reporting.
func (v *VulnerabilityConfig) ResolvedReporting() bool {
	return v.Reporting
}

// validate checks the security section. Declared values are always
// checked so an invalid file can never silently disable or mistime the
// scheduler. Every error names the yaml path.
func (s *SecurityConfig) validate() error {
	if s == nil {
		return nil
	}
	v := s.Vulnerability
	if v == nil {
		return nil
	}
	if v.ScanInterval == InvalidScanInterval {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: security.vulnerability.scan_interval must be a duration\nHint: write a Go duration such as 24h, 12h, or 90m")
	}
	interval, err := v.ScanInterval.Duration()
	if err != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: security.vulnerability.scan_interval %q is not a valid duration\nHint: write a Go duration such as 24h, 12h, or 90m", string(v.ScanInterval))
	}
	if interval < ScanIntervalMinimum {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: security.vulnerability.scan_interval %q is below the %s minimum\nHint: scheduled scans run no faster than every %s", string(v.ScanInterval), ScanIntervalMinimum, ScanIntervalMinimum)
	}
	if v.FailThreshold == InvalidScanSeverity {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: security.vulnerability.fail_threshold must be one of low, medium, high, critical\nHint: findings with unknown severity never trip the threshold; reports always show them")
	}
	if v.FailThreshold != "" && !v.FailThreshold.Valid() {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: security.vulnerability.fail_threshold %q is not one of low, medium, high, critical\nHint: findings with unknown severity never trip the threshold; reports always show them", string(v.FailThreshold))
	}
	return nil
}

// SaveSecurityConfig writes the security section into dir/phelix.yaml
// while preserving every other key, comment, and the file's overall
// formatting: the existing document is edited as a yaml.Node and only the
// `security` mapping is replaced (or appended). A missing file is created
// with just the security section; a malformed existing file is an error,
// never overwritten. The configuration is validated before any write.
func SaveSecurityConfig(dir string, sc *SecurityConfig) error {
	if sc == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "cannot save a nil security config")
	}
	if err := sc.Validate(); err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", path)
		}
		body, merr := yaml.Marshal(map[string]*SecurityConfig{"security": sc})
		if merr != nil {
			return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode security config", merr)
		}
		return writeProjectConfigAtomic(path, append([]byte(projectConfigHeader()), body...))
	}

	var doc yaml.Node
	if uerr := yaml.Unmarshal(raw, &doc); uerr != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, uerr, "%s is malformed; not modifying it", path)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return phelixerr.Newf(phelixerr.CodeConfiguration, "%s is malformed: top level is not a mapping; not modifying it", path)
	}
	root := doc.Content[0]

	newValue := &yaml.Node{}
	if eerr := newValue.Encode(sc); eerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode security config", eerr)
	}

	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Kind == yaml.ScalarNode && root.Content[i].Value == "security" {
			*root.Content[i+1] = *newValue
			replaced = true
			break
		}
	}
	if !replaced {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "security"}
		root.Content = append(root.Content, key, newValue)
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if eerr := enc.Encode(&doc); eerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", eerr)
	}
	if cerr := enc.Close(); cerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", cerr)
	}
	return writeProjectConfigAtomic(path, []byte(buf.String()))
}
