package project

import (
	"fmt"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"go.yaml.in/yaml/v3"
)

// HealthEndpoint is one health check endpoint declared in phelix.yaml.
// It mirrors the fields of the persisted health system (internal/health)
// except that endpoints are declared by path; the full URL is derived from
// the application's port when the configuration is applied.
type HealthEndpoint struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	Interval string `yaml:"interval,omitempty"`
	Retries  int    `yaml:"retries,omitempty"`
	Mode     string `yaml:"mode,omitempty"`
}

// HealthConfig groups the health endpoints declared in phelix.yaml.
type HealthConfig struct {
	Endpoints []HealthEndpoint `yaml:"endpoints"`
}

// DeployConfig selects the deployment strategy used by `phelix rebuild`.
type DeployConfig struct {
	Strategy string `yaml:"strategy,omitempty"`
	Replicas int    `yaml:"replicas,omitempty"`
	// Rollout configures canary/progressive deployments.
	Rollout *RolloutConfig `yaml:"rollout,omitempty"`
}

// RolloutConfig is the phelix.yaml shape of a canary/progressive rollout. A
// one-shot canary uses Canary/Duration; a progressive rollout enumerates its
// Steps. Verification bounds apply to both.
type RolloutConfig struct {
	// Canary is the traffic share (percent) the canary receives before
	// promotion for deploy.strategy: canary. Optional; the deploy default
	// applies when unset.
	Canary *Percent `yaml:"canary,omitempty"`
	// Duration is the canary's verification window (e.g. "2m"). Optional.
	Duration string `yaml:"duration,omitempty"`
	// Steps enumerates a progressive rollout's traffic shares in order. The
	// final step must be 100 (the promotion).
	Steps []RolloutStepConfig `yaml:"steps,omitempty"`
	// Verification bounds how far the canary may deviate from the stable
	// baseline. Optional; deploy defaults apply per field.
	Verification *RolloutVerificationConfig `yaml:"verification,omitempty"`
}

// RolloutStepConfig is one progressive rollout step.
type RolloutStepConfig struct {
	// Traffic is the canary's share of traffic for this step. Accepts either
	// a bare number (25) or a percent string ("25%").
	Traffic Percent `yaml:"traffic"`
	// Duration is how long the canary is observed at this share. Optional;
	// the final promotion step usually omits it.
	Duration string `yaml:"duration,omitempty"`
}

// RolloutVerificationConfig bounds canary-vs-baseline deviations.
type RolloutVerificationConfig struct {
	// Interval is the health/metrics poll cadence during a step window.
	Interval string `yaml:"interval,omitempty"`
	// MaxErrorRate is the absolute canary error-rate cap in percent.
	MaxErrorRate float64 `yaml:"max_error_rate,omitempty"`
	// MaxErrorDelta is how many percentage points the canary error rate may
	// exceed the stable baseline.
	MaxErrorDelta float64 `yaml:"max_error_delta,omitempty"`
	// MaxP95Factor bounds the canary p95 latency relative to the baseline's.
	MaxP95Factor float64 `yaml:"max_p95_factor,omitempty"`
}

// Percent is a YAML percentage that accepts either a bare number (25) or a
// percent-suffixed string ("25%").
type Percent int

// InvalidPercent is the sentinel stored when a percentage could not be parsed.
// Reporting it from validate() (with the yaml path) instead of failing the
// decode keeps the error actionable: yaml v3 swallows the message of errors
// returned by custom unmarshalers, which would degrade everything to an
// opaque "phelix.yaml is malformed".
const InvalidPercent Percent = -1

// UnmarshalYAML implements yaml.Unmarshaler. It never fails: unparseable
// values become InvalidPercent, which Config.validate reports by yaml path.
func (p *Percent) UnmarshalYAML(node *yaml.Node) error {
	*p = InvalidPercent
	if node.Kind != yaml.ScalarNode {
		return nil
	}
	raw := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(node.Value), "%"))
	v, err := parseWholeNumber(strings.TrimSpace(raw))
	if err != nil {
		return nil
	}
	*p = Percent(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler, writing the bare number so a saved
// file round-trips.
func (p Percent) MarshalYAML() (any, error) { return int(p), nil }

// parseWholeNumber parses a non-negative integer without accepting the float
// or underscore forms strconv would.
func parseWholeNumber(s string) (int, error) {
	if s == "" {
		return 0, phelixerr.New(phelixerr.CodeConfiguration, "empty number")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, phelixerr.Newf(phelixerr.CodeConfiguration, "not a whole number: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// Supported deploy strategies (mapped onto the existing deployment paths).
const (
	StrategyClassic     = "classic"
	StrategyBlueGreen   = "blue-green"
	StrategyRolling     = "rolling"
	StrategyCanary      = "canary"
	StrategyProgressive = "progressive"
)

// SupportedHealthModes mirrors health.DeployTierMode values.
var supportedHealthModes = map[string]bool{
	"auto": true, "http": true, "tcp-only": true, "none": true,
}

var supportedStrategies = map[string]bool{
	StrategyClassic: true, StrategyBlueGreen: true, StrategyRolling: true,
	StrategyCanary: true, StrategyProgressive: true,
}

// validate checks the health and deploy sections. Field-level errors name the
// exact yaml path so the user can fix the file without reading source.
func (c *Config) validate() error {
	if c.Deploy != nil {
		s := strings.TrimSpace(c.Deploy.Strategy)
		if s != "" && !supportedStrategies[s] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid deploy.strategy %q\nHint: expected one of: classic, blue-green, rolling, canary, progressive", c.Deploy.Strategy)
		}
		if c.Deploy.Replicas < 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid deploy.replicas %d\nHint: replicas must be >= 1", c.Deploy.Replicas)
		}
		if c.Deploy.Replicas > 0 && s != StrategyRolling {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.replicas requires deploy.strategy: rolling")
		}
		if c.Deploy.Rollout != nil {
			if err := c.Deploy.Rollout.validate(s); err != nil {
				return err
			}
		} else if s == StrategyProgressive {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.strategy: progressive requires a deploy.rollout.steps section\nHint: define at least one canary step and the final 100%% step")
		}
	}

	if c.Health == nil {
		return nil
	}
	seen := make(map[string]bool, len(c.Health.Endpoints))
	for i, ep := range c.Health.Endpoints {
		if strings.TrimSpace(ep.Name) == "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] is missing required field: name", i)
		}
		if seen[ep.Name] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: duplicate health endpoint name %q", ep.Name)
		}
		seen[ep.Name] = true
		if !strings.HasPrefix(ep.Path, "/") {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] (%s) has invalid path %q\nHint: path must start with '/' (e.g. /health)", i, ep.Name, ep.Path)
		}
		if ep.Interval != "" {
			if _, err := time.ParseDuration(ep.Interval); err != nil {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: health.endpoints[%d] (%s) has invalid interval %q\nHint: use a duration like 10s or 1m", i, ep.Name, ep.Interval)
			}
		}
		if ep.Retries < 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] (%s) has invalid retries %d\nHint: retries must be >= 0", i, ep.Name, ep.Retries)
		}
		if ep.Mode != "" && !supportedHealthModes[ep.Mode] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] (%s) has invalid mode %q\nHint: expected one of: auto, http, tcp-only, none", i, ep.Name, ep.Mode)
		}
	}
	return nil
}

// validate checks the rollout section against the declared strategy. Every
// rule maps to a safety property of the rollout engine: shares must be whole
// percentages within 1-100, must strictly increase (a non-increasing plan
// would re-verify traffic that already passed), must end at 100 (a plan
// without the promotion step would leave traffic split forever), and
// durations must parse and be non-negative.
func (r *RolloutConfig) validate(strategy string) error {
	if r == nil {
		return nil
	}
	if strategy != StrategyCanary && strategy != StrategyProgressive {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: deploy.rollout requires deploy.strategy: canary or progressive, got %q",
			strategy)
	}

	if len(r.Steps) > 0 {
		if strategy != StrategyProgressive {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.steps requires deploy.strategy: progressive (a canary rollout has a single share — use deploy.rollout.canary)")
		}
		if len(r.Steps) < 2 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.steps needs at least one canary step and the final 100%% step, got %d step(s)", len(r.Steps))
		}
		prev := 0
		for i, step := range r.Steps {
			path := fmt.Sprintf("deploy.rollout.steps[%d]", i)
			if step.Traffic == InvalidPercent {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s has an unparseable traffic value\nHint: use a whole percentage like 25 or \"25%%\"", path)
			}
			if step.Traffic < 1 || step.Traffic > 100 {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s has invalid traffic %d%%\nHint: each step must be a whole percentage between 1%% and 100%%", path, step.Traffic)
			}
			if int(step.Traffic) <= prev {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: %s has traffic %d%% but the previous step was %d%%\nHint: traffic shares must strictly increase", path, step.Traffic, prev)
			}
			prev = int(step.Traffic)
			if err := validateStepDuration(path, step.Duration); err != nil {
				return err
			}
		}
		if last := r.Steps[len(r.Steps)-1].Traffic; last != 100 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.steps must end with the 100%% promotion step, got %d%%", last)
		}
	}

	if r.Canary != nil {
		if strategy != StrategyCanary {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.canary requires deploy.strategy: canary")
		}
		if *r.Canary == InvalidPercent {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.canary has an unparseable value\nHint: use a whole percentage like 5 or \"5%%\"")
		}
		if *r.Canary < 1 || *r.Canary > 99 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.canary must be between 1%% and 99%%, got %d%%", *r.Canary)
		}
	}
	if err := validateStepDuration("deploy.rollout.duration", r.Duration); err != nil {
		return err
	}

	if r.Verification != nil {
		v := r.Verification
		if v.Interval != "" {
			d, err := time.ParseDuration(v.Interval)
			if err != nil || d <= 0 {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: deploy.rollout.verification.interval must be a positive duration like 5s, got %q", v.Interval)
			}
		}
		if v.MaxErrorRate < 0 || v.MaxErrorRate > 100 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.verification.max_error_rate must be between 0 and 100, got %v", v.MaxErrorRate)
		}
		if v.MaxErrorDelta < 0 || v.MaxErrorDelta > 100 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.verification.max_error_delta must be between 0 and 100, got %v", v.MaxErrorDelta)
		}
		if v.MaxP95Factor != 0 && v.MaxP95Factor < 1 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout.verification.max_p95_factor must be >= 1, got %v", v.MaxP95Factor)
		}
	}
	return nil
}

func validateStepDuration(path, value string) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: %s has invalid duration %q\nHint: use a duration like 30s or 2m", path, value)
	}
	if d < 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: %s must not be negative, got %s", path, value)
	}
	return nil
}
