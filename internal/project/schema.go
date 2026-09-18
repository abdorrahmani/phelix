package project

import (
	"fmt"
	"os"
	"path/filepath"
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
	// Autoscaling configures the rolling-deploy replica autoscaling decision
	// engine. Optional; apps without the block never autoscale.
	Autoscaling *AutoscalingConfig `yaml:"autoscaling,omitempty"`
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

// AutoscalingConfig is the phelix.yaml shape of deploy.autoscaling, the
// rolling-deploy replica autoscaling block. Phase 1 only loads, validates and
// resolves it: the decision engine (internal/deploy/autoscale.go) consumes the
// resolved AutoscaleSettings, and nothing executes scaling decisions yet.
type AutoscalingConfig struct {
	Enabled           bool                    `yaml:"enabled,omitempty"`
	MinReplicas       int                     `yaml:"min_replicas,omitempty"`
	MaxReplicas       int                     `yaml:"max_replicas,omitempty"`
	Interval          string                  `yaml:"interval,omitempty"`
	Cooldown          string                  `yaml:"cooldown,omitempty"`
	CPU               *AutoscaleCPUConfig     `yaml:"cpu,omitempty"`
	Latency           *AutoscaleLatencyConfig `yaml:"latency,omitempty"`
	EvaluationWindows int                     `yaml:"evaluation_windows,omitempty"`
}

// AutoscaleCPUConfig holds the CPU thresholds in percent. Zero means unset.
type AutoscaleCPUConfig struct {
	ScaleUp   float64 `yaml:"scale_up,omitempty"`
	ScaleDown float64 `yaml:"scale_down,omitempty"`
}

// AutoscaleLatencyConfig holds the proxy p95 latency thresholds. Empty means
// unset.
type AutoscaleLatencyConfig struct {
	ScaleUpP95   string `yaml:"scale_up_p95,omitempty"`
	ScaleDownP95 string `yaml:"scale_down_p95,omitempty"`
}

// Defaults applied by AutoscalingConfig.Resolve for fields left unset (0 or
// ""). Validation rejects negatives and unparseable values, so those are
// unambiguous.
const (
	DefaultAutoscaleInterval         = 15 * time.Second
	DefaultAutoscaleCooldown         = 60 * time.Second
	DefaultAutoscaleCPUUpPercent     = 70.0
	DefaultAutoscaleCPUDownPercent   = 30.0
	DefaultAutoscaleP95Up            = 500 * time.Millisecond
	DefaultAutoscaleP95Down          = 150 * time.Millisecond
	DefaultAutoscaleEvaluationWindow = 1
)

// AutoscaleSettings is the autoscaling block fully resolved: defaults applied,
// durations parsed. It is the input to the deploy package's decision engine
// (deploy.NewAutoscaler).
type AutoscaleSettings struct {
	Enabled           bool
	MinReplicas       int
	MaxReplicas       int
	Interval          time.Duration
	Cooldown          time.Duration
	ScaleUpCPU        float64
	ScaleDownCPU      float64
	ScaleUpP95        time.Duration
	ScaleDownP95      time.Duration
	EvaluationWindows int
}

// Resolve applies the documented defaults. replicas is deploy.replicas; an
// unset max_replicas defaults to it so enabling autoscaling alone never
// widens the replica set beyond what the user declared. Call only on a config
// that passed validate: unparseable values fall back to defaults here.
func (a *AutoscalingConfig) Resolve(replicas int) AutoscaleSettings {
	if a == nil {
		return AutoscaleSettings{}
	}
	s := AutoscaleSettings{Enabled: a.Enabled}
	s.MinReplicas = a.MinReplicas
	if s.MinReplicas < 1 {
		s.MinReplicas = 1
	}
	s.MaxReplicas = a.MaxReplicas
	if s.MaxReplicas < s.MinReplicas {
		if replicas > s.MinReplicas {
			s.MaxReplicas = replicas
		} else {
			s.MaxReplicas = s.MinReplicas
		}
	}
	s.Interval = positiveDurationOrDefault(a.Interval, DefaultAutoscaleInterval)
	s.Cooldown = nonNegativeDurationOrDefault(a.Cooldown, DefaultAutoscaleCooldown)
	s.ScaleUpCPU = DefaultAutoscaleCPUUpPercent
	s.ScaleDownCPU = DefaultAutoscaleCPUDownPercent
	if a.CPU != nil {
		if a.CPU.ScaleUp > 0 {
			s.ScaleUpCPU = a.CPU.ScaleUp
		}
		// ponytail: a literal 0 reads as "unset" and becomes the 30% default;
		// switch to a pointer field if an "idle-only" scale_down of 0 is ever needed.
		if a.CPU.ScaleDown > 0 {
			s.ScaleDownCPU = a.CPU.ScaleDown
		}
	}
	s.ScaleUpP95 = DefaultAutoscaleP95Up
	s.ScaleDownP95 = DefaultAutoscaleP95Down
	if a.Latency != nil {
		if d, err := time.ParseDuration(a.Latency.ScaleUpP95); err == nil && d > 0 {
			s.ScaleUpP95 = d
		}
		if d, err := time.ParseDuration(a.Latency.ScaleDownP95); err == nil && d > 0 {
			s.ScaleDownP95 = d
		}
	}
	s.EvaluationWindows = DefaultAutoscaleEvaluationWindow
	if a.EvaluationWindows > 0 {
		s.EvaluationWindows = a.EvaluationWindows
	}
	return s
}

func positiveDurationOrDefault(value string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func nonNegativeDurationOrDefault(value string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return def
	}
	return d
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

// Values of the top-level watching key: enable opts the app into backend
// monitoring, disable keeps it out. `phelix init` writes the disable default.
const (
	WatchingEnable  = "enable"
	WatchingDisable = "disable"
)

// WatchingSetting reports the watching state declared in phelix.yaml. ok is
// false when the key is absent, in which case callers must leave the app's
// persisted flag untouched (older projects predate the key; the persisted
// default — disabled — already applies to them). A nil Config (no phelix.yaml
// was loaded) is the same absence, reported safely: loadProjectConfig returns
// (nil, nil) for a missing file, and a build must never crash on it.
func (c *Config) WatchingSetting() (enabled, ok bool) {
	if c == nil {
		return false, false
	}
	switch c.Watching {
	case WatchingEnable:
		return true, true
	case WatchingDisable:
		return false, true
	}
	return false, false
}

// SetWatching writes the watching key into dir/phelix.yaml so the project
// file reflects a `phelix watch` toggle: the next build/rebuild converges on
// the yaml (syncProjectWatching), which would otherwise silently revert the
// runtime toggle.
//
// The existing document is edited as a yaml.Node — only the `watching` value
// is replaced (or the key appended); every other key, comment, and the file's
// formatting survive, exactly like SaveMatrixConfig. A missing file is
// reported as CodeNotFound and never created — creating a project config is
// `phelix init`'s job, not a runtime toggle's. A malformed file is an error
// and is never modified.
func SetWatching(dir string, enabled bool) error {
	path := filepath.Join(dir, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return phelixerr.Newf(phelixerr.CodeNotFound, "%s not found in %s", FileName, dir)
		}
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", path)
	}

	var doc yaml.Node
	if uerr := yaml.Unmarshal(raw, &doc); uerr != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, uerr, "%s is malformed; not modifying it", path)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return phelixerr.Newf(phelixerr.CodeConfiguration, "%s is malformed: top level is not a mapping; not modifying it", path)
	}
	root := doc.Content[0]

	value := WatchingDisable
	if enabled {
		value = WatchingEnable
	}
	newValue := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}

	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Kind == yaml.ScalarNode && root.Content[i].Value == "watching" {
			// Keep the key node (its comments survive); swap only the value.
			*root.Content[i+1] = *newValue
			replaced = true
			break
		}
	}
	if !replaced {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "watching"}
		root.Content = append(root.Content, key, newValue)
	}

	// Marshal the document node (not the root mapping): the file's leading
	// comments live on the document node and would be dropped otherwise.
	// Indent 2 keeps the common phelix.yaml style.
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if eerr := enc.Encode(&doc); eerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", eerr)
	}
	if cerr := enc.Close(); cerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", cerr)
	}
	if werr := os.WriteFile(path, []byte(buf.String()), 0o644); werr != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, werr, "failed to write %s", path)
	}
	return nil
}

// SupportedHealthModes mirrors health.DeployTierMode values.
var supportedHealthModes = map[string]bool{
	"auto": true, "http": true, "tcp-only": true, "none": true,
}

var supportedStrategies = map[string]bool{
	StrategyClassic: true, StrategyBlueGreen: true, StrategyRolling: true,
	StrategyCanary: true, StrategyProgressive: true,
}

// validate checks the watching, health and deploy sections. Field-level errors
// name the exact yaml path so the user can fix the file without reading source.
func (c *Config) validate() error {
	if _, ok := c.WatchingSetting(); !ok && strings.TrimSpace(c.Watching) != "" {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: invalid watching %q\nHint: expected one of: enable, disable", c.Watching)
	}

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
		if c.Deploy.Autoscaling != nil {
			if err := c.Deploy.Autoscaling.validate(s); err != nil {
				return err
			}
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

// validate checks the autoscaling block. Parse and range errors are reported
// even when the block is disabled so typos surface immediately; the strategy
// requirement applies only when autoscaling is enabled (a disabled block on a
// classic deploy is inert, not an error).
func (a *AutoscalingConfig) validate(strategy string) error {
	if a == nil {
		return nil
	}
	if a.Enabled && strategy != StrategyRolling {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: deploy.autoscaling requires deploy.strategy: rolling, got %q\nHint: autoscaling manages rolling replicas", strategy)
	}
	if a.MinReplicas < 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: invalid deploy.autoscaling.min_replicas %d\nHint: min_replicas must be >= 1", a.MinReplicas)
	}
	if a.MaxReplicas < 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: invalid deploy.autoscaling.max_replicas %d\nHint: max_replicas must be >= 1", a.MaxReplicas)
	}
	if a.MinReplicas > 0 && a.MaxReplicas > 0 && a.MinReplicas > a.MaxReplicas {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: deploy.autoscaling.min_replicas %d exceeds max_replicas %d", a.MinReplicas, a.MaxReplicas)
	}
	if err := validateStepDuration("deploy.autoscaling.interval", a.Interval); err != nil {
		return err
	}
	if err := validateStepDuration("deploy.autoscaling.cooldown", a.Cooldown); err != nil {
		return err
	}
	if a.EvaluationWindows < 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: invalid deploy.autoscaling.evaluation_windows %d\nHint: evaluation_windows must be >= 1", a.EvaluationWindows)
	}
	if a.CPU != nil {
		if a.CPU.ScaleUp < 0 || a.CPU.ScaleUp > 100 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.autoscaling.cpu.scale_up must be between 0 and 100, got %v", a.CPU.ScaleUp)
		}
		if a.CPU.ScaleDown < 0 || a.CPU.ScaleDown > 100 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.autoscaling.cpu.scale_down must be between 0 and 100, got %v", a.CPU.ScaleDown)
		}
		if a.CPU.ScaleUp > 0 && a.CPU.ScaleDown > 0 && a.CPU.ScaleDown >= a.CPU.ScaleUp {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.autoscaling.cpu.scale_down (%v) must be below cpu.scale_up (%v)", a.CPU.ScaleDown, a.CPU.ScaleUp)
		}
	}
	if a.Latency != nil {
		up, err := parseAutoscaleP95("deploy.autoscaling.latency.scale_up_p95", a.Latency.ScaleUpP95)
		if err != nil {
			return err
		}
		down, err := parseAutoscaleP95("deploy.autoscaling.latency.scale_down_p95", a.Latency.ScaleDownP95)
		if err != nil {
			return err
		}
		if up > 0 && down > 0 && down >= up {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.autoscaling.latency.scale_down_p95 (%s) must be below latency.scale_up_p95 (%s)", a.Latency.ScaleDownP95, a.Latency.ScaleUpP95)
		}
	}
	return nil
}

// parseAutoscaleP95 parses one optional latency threshold: empty means unset
// (the default applies at Resolve); anything else must parse as a
// non-negative duration.
func parseAutoscaleP95(path, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: %s has invalid duration %q\nHint: use a duration like 500ms or 2s", path, value)
	}
	if d < 0 {
		return 0, phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: %s must not be negative, got %s", path, value)
	}
	return d, nil
}
