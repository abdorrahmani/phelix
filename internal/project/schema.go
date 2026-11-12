package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	Name          string `yaml:"name"`
	Path          string `yaml:"path"`
	Interval      string `yaml:"interval,omitempty"`
	Retries       int    `yaml:"retries,omitempty"`
	Mode          string `yaml:"mode,omitempty"`
	ExpectedCodes string `yaml:"codes,omitempty"`
	Timeout       string `yaml:"timeout,omitempty"`
}

// HealthConfig groups the health endpoints declared in phelix.yaml.
type HealthConfig struct {
	Endpoints []HealthEndpoint `yaml:"endpoints"`
	// Replace opts into discarding endpoints the file does not declare.
	// Without it the file merges: declared names are added/updated, and the
	// endpoints configured some other way (`phelix health add`) survive.
	Replace bool `yaml:"replace,omitempty"`
	// Mode selects the worker health mode: "process" (watch the process
	// identity) or "exec" (run a command). Only valid with type: worker;
	// endpoint probes are port-based and meaningless for a port-less worker.
	Mode string `yaml:"mode,omitempty"`
	// Exec configures the exec probe when Mode is "exec".
	Exec *ExecHealthConfig `yaml:"exec,omitempty"`
}

// DeployConfig selects the deployment strategy used by `phelix rebuild`.
type DeployConfig struct {
	Strategy string `yaml:"strategy,omitempty"`
	Replicas int    `yaml:"replicas,omitempty"`
	// Runtime selects how instances are launched: "native" (default — exec the
	// built binary as a host process, the model that runs on a bare VPS) or
	// "docker" (build an image and run each instance as a container, so one
	// Phelix agent on the host manages many app containers as one server). The
	// PHELIX_RUNTIME env var overrides this per invocation. Docker runtime
	// requires a zero-downtime strategy (blue-green/rolling/canary/progressive);
	// the classic stop→start path stays native-only.
	Runtime string `yaml:"runtime,omitempty"`
	// Network is the user-defined Docker network each managed app container is
	// attached to, so the app resolves backing services (mysql, redis, ...) by
	// their docker-compose DNS names (mysql:3306, redis:6379). Only meaningful
	// with runtime: docker; empty means the default bridge (no --network passed,
	// the pre-network behavior). Resolved via DeployNetwork().
	Network string `yaml:"network,omitempty"`
	// Docker configures HOW the app image is built under runtime: docker (the
	// launcher, proxy, network, blue-green and rollback are unaffected — this is
	// only where the image comes from). Only meaningful with runtime: docker.
	Docker *DockerRuntimeConfig `yaml:"docker,omitempty"`
	// Rollout configures canary/progressive deployments.
	Rollout *RolloutConfig `yaml:"rollout,omitempty"`
	// Analysis configures post-deployment impact analysis. Optional; absent
	// means enabled with the built-in windows and thresholds.
	Analysis *DeployAnalysisConfig `yaml:"analysis,omitempty"`
	// Autoscaling configures the rolling-deploy replica autoscaling decision
	// engine. Optional; apps without the block never autoscale.
	Autoscaling *AutoscalingConfig `yaml:"autoscaling,omitempty"`
	// Rolling holds the worker rolling-replacement parameters (surge,
	// unavailable, drain timeout). Only valid with type: worker.
	Rolling *RollingDeployConfig `yaml:"rolling,omitempty"`
}

// DockerRuntimeConfig selects HOW the app image is built under
// deploy.runtime: docker. It changes only where the image comes from; the
// launcher, proxy, network attach, blue-green and rollback are identical
// regardless of strategy.
type DockerRuntimeConfig struct {
	// Build is the image-build strategy: "dockerfile" (default — generate or
	// reuse a Dockerfile and docker-build it, today's behavior) or "compose"
	// (build the named compose service with its own build config, then tag the
	// result <app>:vN). Empty means dockerfile.
	Build string `yaml:"build,omitempty"`
	// ComposeFile is the compose file to build from when Build == "compose".
	// Defaults to "docker-compose.yml", resolved relative to the build dir.
	ComposeFile string `yaml:"compose_file,omitempty"`
	// Service is the compose service whose build config produces this app's
	// image. Required when Build == "compose".
	Service string `yaml:"service,omitempty"`
}

// Docker image-build strategies for deploy.docker.build.
const (
	DockerBuildDockerfile = "dockerfile"
	DockerBuildCompose    = "compose"
)

// DefaultComposeFile is the compose file used when deploy.docker.compose_file
// is unset.
const DefaultComposeFile = "docker-compose.yml"

var supportedDockerBuilds = map[string]bool{
	DockerBuildDockerfile: true, DockerBuildCompose: true,
}

// DeployAnalysisConfig is the phelix.yaml shape of deploy.analysis, the
// post-deployment impact analysis settings. The zero value of every field
// means "use the built-in default", so an empty block is harmless.
type DeployAnalysisConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"`
	// BaselineWindow is how far back from the deployment the stable baseline is
	// measured ("5m" by default).
	BaselineWindow string `yaml:"baseline_window,omitempty"`
	// ObserveWindow is how long after promotion the new version is observed
	// ("2m" by default).
	ObserveWindow string `yaml:"observe_window,omitempty"`
	// MinSamples is the minimum sample count required per window and metric
	// family before a comparison is considered meaningful (5 by default).
	MinSamples int `yaml:"min_samples,omitempty"`
	// ErrorRateMaxDelta is the allowed error-rate increase in percentage points
	// before it counts as a regression (1.0 by default).
	ErrorRateMaxDelta float64 `yaml:"error_rate_max_delta,omitempty"`
	// LatencyMaxPercent is the allowed P95 latency increase percentage (20 by
	// default).
	LatencyMaxPercent float64 `yaml:"latency_max_percent,omitempty"`
	// CPUMaxPercent is the allowed average CPU increase percentage (25 by
	// default).
	CPUMaxPercent float64 `yaml:"cpu_max_percent,omitempty"`
	// MemoryMaxPercent is the allowed average memory increase percentage (15 by
	// default).
	MemoryMaxPercent float64 `yaml:"memory_max_percent,omitempty"`
	// RestartMaxExtra is the number of additional restarts tolerated after the
	// deployment (0 by default).
	RestartMaxExtra int `yaml:"restart_max_extra,omitempty"`
	// ThroughputMaxDegradation is the allowed throughput drop percentage (20 by
	// default).
	ThroughputMaxDegradation float64 `yaml:"throughput_max_degradation,omitempty"`
	// HealthFailuresMaxExtra is the number of additional failed health checks
	// tolerated after the deployment (0 by default).
	HealthFailuresMaxExtra int `yaml:"health_failures_max_extra,omitempty"`
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
// replica autoscaling block for rolling services and workers. The controller
// in internal/autoscale consumes the resolved AutoscaleSettings and executes
// decisions through the existing scaling paths.
type AutoscalingConfig struct {
	Enabled           bool                    `yaml:"enabled,omitempty"`
	MinReplicas       int                     `yaml:"min_replicas,omitempty"`
	MaxReplicas       int                     `yaml:"max_replicas,omitempty"`
	Interval          string                  `yaml:"interval,omitempty"`
	Cooldown          string                  `yaml:"cooldown,omitempty"`
	CPU               *AutoscaleCPUConfig     `yaml:"cpu,omitempty"`
	Latency           *AutoscaleLatencyConfig `yaml:"latency,omitempty"`
	EvaluationWindows int                     `yaml:"evaluation_windows,omitempty"`
	// Metrics is the extended metric block. When a metric is declared here it
	// takes precedence over the legacy top-level cpu/latency fields, which
	// remain valid for backward compatibility.
	Metrics *AutoscaleMetricsConfig `yaml:"metrics,omitempty"`
	// ScaleUp and ScaleDown override the evaluation cadence per direction.
	// The legacy top-level cooldown/evaluation_windows fields remain valid
	// shorthand that populates both directions.
	ScaleUp   *AutoscaleDirectionConfig `yaml:"scale_up,omitempty"`
	ScaleDown *AutoscaleDirectionConfig `yaml:"scale_down,omitempty"`
}

// AutoscaleCPUConfig holds a scale_up/scale_down threshold pair. Threshold
// semantics differ per metric (CPU and memory are percents, RPS is requests
// per second), but the pair shape is shared. Zero means unset: a metric with
// no scale_up threshold never contributes a scale-up signal, and one with no
// scale_down threshold never participates in the scale-down conjunction.
type AutoscaleCPUConfig struct {
	ScaleUp   float64 `yaml:"scale_up,omitempty"`
	ScaleDown float64 `yaml:"scale_down,omitempty"`
}

// AutoscaleLatencyConfig holds the proxy latency thresholds. Empty means
// unset. P99 is optional; when unset only P95 participates.
type AutoscaleLatencyConfig struct {
	ScaleUpP95   string `yaml:"scale_up_p95,omitempty"`
	ScaleDownP95 string `yaml:"scale_down_p95,omitempty"`
	ScaleUpP99   string `yaml:"scale_up_p99,omitempty"`
	ScaleDownP99 string `yaml:"scale_down_p99,omitempty"`
}

// AutoscaleMetricsConfig groups the per-metric thresholds under
// deploy.autoscaling.metrics.
type AutoscaleMetricsConfig struct {
	CPU               *AutoscaleCPUConfig     `yaml:"cpu,omitempty"`
	Memory            *AutoscaleCPUConfig     `yaml:"memory,omitempty"`
	RequestsPerSecond *AutoscaleCPUConfig     `yaml:"requests_per_second,omitempty"`
	Latency           *AutoscaleLatencyConfig `yaml:"latency,omitempty"`
}

// AutoscaleDirectionConfig holds the per-direction cadence overrides under
// deploy.autoscaling.scale_up and deploy.autoscaling.scale_down.
// StabilizationWindow is only consumed by the scale-down direction.
type AutoscaleDirectionConfig struct {
	EvaluationWindows   int    `yaml:"evaluation_windows,omitempty"`
	Cooldown            string `yaml:"cooldown,omitempty"`
	StabilizationWindow string `yaml:"stabilization_window,omitempty"`
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
	Enabled     bool
	MinReplicas int
	MaxReplicas int
	Interval    time.Duration
	// Cooldown and EvaluationWindows are the legacy top-level values, kept
	// populated for compatibility; the per-direction fields below take
	// precedence in the engine.
	Cooldown          time.Duration
	ScaleUpCPU        float64
	ScaleDownCPU      float64
	ScaleUpP95        time.Duration
	ScaleDownP95      time.Duration
	EvaluationWindows int

	// Memory thresholds in percent of the memory baseline. Zero means the
	// metric does not participate in that direction.
	ScaleUpMemory   float64
	ScaleDownMemory float64
	// Requests-per-second thresholds. Zero means the metric does not
	// participate in that direction.
	ScaleUpRPS   float64
	ScaleDownRPS float64
	// P99 latency thresholds. Zero means P99 does not participate.
	ScaleUpP99   time.Duration
	ScaleDownP99 time.Duration

	// Per-direction cadence. The engine uses these; the legacy fields above
	// mirror them for callers that predate the split.
	ScaleUpWindows      int
	ScaleDownWindows    int
	ScaleUpCooldown     time.Duration
	ScaleDownCooldown   time.Duration
	StabilizationWindow time.Duration
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
	s.ScaleUpP95 = DefaultAutoscaleP95Up
	s.ScaleDownP95 = DefaultAutoscaleP95Down
	s.EvaluationWindows = DefaultAutoscaleEvaluationWindow
	if a.EvaluationWindows > 0 {
		s.EvaluationWindows = a.EvaluationWindows
	}

	// The legacy top-level cpu/latency fields remain the baseline; the metrics
	// block overrides per metric when present. A literal 0 threshold reads as
	// "unset" (the metric does not participate), never as a zero target.
	cpu := a.CPU
	mem := (*AutoscaleCPUConfig)(nil)
	rps := (*AutoscaleCPUConfig)(nil)
	lat := a.Latency
	if a.Metrics != nil {
		if a.Metrics.CPU != nil {
			cpu = a.Metrics.CPU
		}
		mem = a.Metrics.Memory
		rps = a.Metrics.RequestsPerSecond
		if a.Metrics.Latency != nil {
			lat = a.Metrics.Latency
		}
	}
	if cpu != nil {
		if cpu.ScaleUp > 0 {
			s.ScaleUpCPU = cpu.ScaleUp
		}
		if cpu.ScaleDown > 0 {
			s.ScaleDownCPU = cpu.ScaleDown
		}
	}
	if mem != nil {
		if mem.ScaleUp > 0 {
			s.ScaleUpMemory = mem.ScaleUp
		}
		if mem.ScaleDown > 0 {
			s.ScaleDownMemory = mem.ScaleDown
		}
	}
	if rps != nil {
		if rps.ScaleUp > 0 {
			s.ScaleUpRPS = rps.ScaleUp
		}
		if rps.ScaleDown > 0 {
			s.ScaleDownRPS = rps.ScaleDown
		}
	}
	if lat != nil {
		if d, err := time.ParseDuration(lat.ScaleUpP95); err == nil && d > 0 {
			s.ScaleUpP95 = d
		}
		if d, err := time.ParseDuration(lat.ScaleDownP95); err == nil && d > 0 {
			s.ScaleDownP95 = d
		}
		if d, err := time.ParseDuration(lat.ScaleUpP99); err == nil && d > 0 {
			s.ScaleUpP99 = d
		}
		if d, err := time.ParseDuration(lat.ScaleDownP99); err == nil && d > 0 {
			s.ScaleDownP99 = d
		}
	}

	s.ScaleUpWindows = s.EvaluationWindows
	s.ScaleDownWindows = s.EvaluationWindows
	s.ScaleUpCooldown = s.Cooldown
	s.ScaleDownCooldown = s.Cooldown
	if a.ScaleUp != nil {
		if a.ScaleUp.EvaluationWindows > 0 {
			s.ScaleUpWindows = a.ScaleUp.EvaluationWindows
		}
		if d, err := time.ParseDuration(a.ScaleUp.Cooldown); err == nil && d >= 0 {
			s.ScaleUpCooldown = d
		}
	}
	if a.ScaleDown != nil {
		if a.ScaleDown.EvaluationWindows > 0 {
			s.ScaleDownWindows = a.ScaleDown.EvaluationWindows
		}
		if d, err := time.ParseDuration(a.ScaleDown.Cooldown); err == nil && d >= 0 {
			s.ScaleDownCooldown = d
		}
		if d, err := time.ParseDuration(a.ScaleDown.StabilizationWindow); err == nil && d > 0 {
			s.StabilizationWindow = d
		}
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

// Supported deploy runtimes. Native execs the built binary as a host process
// (the model that runs on a bare VPS); docker builds an image and runs each
// instance as a container so one host-level Phelix agent manages many app
// containers as a single server.
const (
	RuntimeNative = "native"
	RuntimeDocker = "docker"
)

var supportedRuntimes = map[string]bool{
	RuntimeNative: true, RuntimeDocker: true,
}

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

// DeployRuntime resolves the effective launch runtime for this build/rebuild.
// Resolution order mirrors the rest of Phelix's config precedence:
//
//	PHELIX_RUNTIME env  →  phelix.yaml deploy.runtime  →  native (default)
//
// The env override lets an operator flip a single invocation to docker without
// editing the file (the same shape as PHELIX_MODE/PHELIX_API overrides in
// config.go). An unrecognized value falls back to native rather than failing —
// validate() already rejects an invalid deploy.runtime in the file, and an env
// typo must never silently switch a deploy to a runtime the operator did not
// mean. A nil Config (no phelix.yaml) resolves to native.
func (c *Config) DeployRuntime() string {
	if env := strings.TrimSpace(os.Getenv("PHELIX_RUNTIME")); env != "" {
		if supportedRuntimes[env] {
			return env
		}
	}
	if c != nil && c.Deploy != nil {
		if rt := strings.TrimSpace(c.Deploy.Runtime); supportedRuntimes[rt] {
			return rt
		}
	}
	return RuntimeNative
}

// DeployNetwork resolves the user-defined Docker network to attach managed app
// containers to. Resolution mirrors DeployRuntime:
//
//	PHELIX_NETWORK env  →  phelix.yaml deploy.network  →  "" (no explicit network)
//
// Empty means "do not pass --network": the container lands on the default
// bridge, exactly the pre-network behavior. Phelix never invents a network
// name. Only meaningful under the docker runtime (validate() rejects a network
// set for any other runtime); a nil Config resolves to "".
func (c *Config) DeployNetwork() string {
	if env := strings.TrimSpace(os.Getenv("PHELIX_NETWORK")); env != "" {
		return env
	}
	if c != nil && c.Deploy != nil {
		if n := strings.TrimSpace(c.Deploy.Network); n != "" {
			return n
		}
	}
	return ""
}

// DeployDockerBuild resolves the effective docker image-build strategy from
// phelix.yaml deploy.docker.build, defaulting to "dockerfile". Config-driven
// (no env override): switching how images are built is a project decision, not
// a per-invocation one. Only meaningful under the docker runtime; a nil Config
// or absent block resolves to "dockerfile".
func (c *Config) DeployDockerBuild() string {
	if c != nil && c.Deploy != nil && c.Deploy.Docker != nil {
		if b := strings.TrimSpace(c.Deploy.Docker.Build); supportedDockerBuilds[b] {
			return b
		}
	}
	return DockerBuildDockerfile
}

// DeployComposeFile resolves the compose file used when build: compose,
// defaulting to DefaultComposeFile. The path is resolved relative to the build
// directory by the caller.
func (c *Config) DeployComposeFile() string {
	if c != nil && c.Deploy != nil && c.Deploy.Docker != nil {
		if f := strings.TrimSpace(c.Deploy.Docker.ComposeFile); f != "" {
			return f
		}
	}
	return DefaultComposeFile
}

// DeployComposeService returns the compose service that builds this app's image
// under build: compose, or "" when none is configured.
func (c *Config) DeployComposeService() string {
	if c != nil && c.Deploy != nil && c.Deploy.Docker != nil {
		return strings.TrimSpace(c.Deploy.Docker.Service)
	}
	return ""
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

// validExpectedCodes reports whether s is in the form the health checker
// understands: "200-299" or a comma list like "200,201,404".
func validExpectedCodes(s string) bool {
	parse := func(v string) bool {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return err == nil && n >= 100 && n <= 599
	}
	if strings.Contains(s, "-") {
		parts := strings.Split(s, "-")
		return len(parts) == 2 && parse(parts[0]) && parse(parts[1])
	}
	for part := range strings.SplitSeq(s, ",") {
		if !parse(part) {
			return false
		}
	}
	return true
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
	if err := c.validateType(); err != nil {
		return err
	}
	switch {
	case c.IsWorker():
		if err := c.validateWorker(); err != nil {
			return err
		}
	case c.IsJob():
		if err := c.validateJob(); err != nil {
			return err
		}
	default:
		if err := c.validateServiceRejectsWorkerKeys(); err != nil {
			return err
		}
	}
	if !c.IsJob() {
		if err := c.validateRejectsJobKeys(); err != nil {
			return err
		}
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
		if rt := strings.TrimSpace(c.Deploy.Runtime); rt != "" && !supportedRuntimes[rt] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid deploy.runtime %q\nHint: expected one of: native, docker", c.Deploy.Runtime)
		}
		// Docker runtime rides the zero-downtime topology (blue-green/rolling/
		// canary/progressive). The classic stop→start path is native-only, so a
		// docker+classic combination is rejected up front rather than silently
		// falling back to a host process.
		if strings.TrimSpace(c.Deploy.Runtime) == RuntimeDocker && s == StrategyClassic {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.runtime: docker requires a zero-downtime strategy\nHint: set deploy.strategy to blue-green, rolling, canary, or progressive")
		}

		if net := strings.TrimSpace(c.Deploy.Network); net != "" {
			if strings.TrimSpace(c.Deploy.Runtime) != RuntimeDocker {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: deploy.network requires deploy.runtime: docker\nHint: set deploy.runtime: docker, or remove deploy.network")
			}
		} else if c.Deploy.Network != "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.network must not be blank\nHint: set a Docker network name (e.g. myproj_appnet) or remove the key")
		}

		if c.Deploy.Docker != nil {
			if strings.TrimSpace(c.Deploy.Runtime) != RuntimeDocker {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: deploy.docker requires deploy.runtime: docker\nHint: set deploy.runtime: docker, or remove the deploy.docker block")
			}
			build := strings.TrimSpace(c.Deploy.Docker.Build)
			if build != "" && !supportedDockerBuilds[build] {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: invalid deploy.docker.build %q\nHint: expected one of: dockerfile, compose", c.Deploy.Docker.Build)
			}
			if build == DockerBuildCompose && strings.TrimSpace(c.Deploy.Docker.Service) == "" {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: deploy.docker.build: compose requires deploy.docker.service\nHint: name the compose service that builds this app's image (e.g. service: app)")
			}
		}
		if c.Deploy.Replicas > 0 && s != StrategyRolling && !c.IsWorker() {
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
			if err := c.Deploy.Autoscaling.validate(s, c.IsWorker()); err != nil {
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
		if ep.Timeout != "" {
			if _, err := time.ParseDuration(ep.Timeout); err != nil {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: health.endpoints[%d] (%s) has invalid timeout %q\nHint: use a duration like 5s or 30s", i, ep.Name, ep.Timeout)
			}
		}

		if ep.ExpectedCodes != "" && !validExpectedCodes(ep.ExpectedCodes) {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] (%s) has invalid codes %q\nHint: use a range like 200-299 or a comma list like 200,201", i, ep.Name, ep.ExpectedCodes)
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
func (a *AutoscalingConfig) validate(strategy string, isWorker bool) error {
	if a == nil {
		return nil
	}
	// Rolling services scale their proxy-routed replicas; workers scale their
	// supervised replica set. Both run the same decision policy. A worker on
	// the classic strategy is allowed: scaling a worker's replica set is
	// independent of its deploy strategy.
	if a.Enabled && strategy != StrategyRolling && !isWorker {
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
		if a.Latency.ScaleUpP99 != "" || a.Latency.ScaleDownP99 != "" {
			up99, err := parseAutoscaleP95("deploy.autoscaling.latency.scale_up_p99", a.Latency.ScaleUpP99)
			if err != nil {
				return err
			}
			down99, err := parseAutoscaleP95("deploy.autoscaling.latency.scale_down_p99", a.Latency.ScaleDownP99)
			if err != nil {
				return err
			}
			if up99 > 0 && down99 > 0 && down99 >= up99 {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: deploy.autoscaling.latency.scale_down_p99 (%s) must be below latency.scale_up_p99 (%s)", a.Latency.ScaleDownP99, a.Latency.ScaleUpP99)
			}
		}
	}
	if a.Metrics != nil {
		if err := a.Metrics.validate(); err != nil {
			return err
		}
	}
	for _, d := range []struct {
		path string
		cfg  *AutoscaleDirectionConfig
	}{
		{"deploy.autoscaling.scale_up", a.ScaleUp},
		{"deploy.autoscaling.scale_down", a.ScaleDown},
	} {
		if d.cfg == nil {
			continue
		}
		if d.cfg.EvaluationWindows < 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid %s.evaluation_windows %d\nHint: evaluation_windows must be >= 1", d.path, d.cfg.EvaluationWindows)
		}
		if err := validateStepDuration(d.path+".cooldown", d.cfg.Cooldown); err != nil {
			return err
		}
		if err := validateStepDuration(d.path+".stabilization_window", d.cfg.StabilizationWindow); err != nil {
			return err
		}
	}
	return nil
}

// validate checks the metrics block thresholds. CPU and memory are percents
// bounded to 0..100 with down strictly below up; RPS is a non-negative pair
// with down below up; latency bounds reuse the p95 parser.
func (m *AutoscaleMetricsConfig) validate() error {
	if m == nil {
		return nil
	}
	pairs := []struct {
		path string
		cfg  *AutoscaleCPUConfig
	}{
		{"deploy.autoscaling.metrics.cpu", m.CPU},
		{"deploy.autoscaling.metrics.memory", m.Memory},
	}
	for _, p := range pairs {
		if p.cfg == nil {
			continue
		}
		if p.cfg.ScaleUp < 0 || p.cfg.ScaleUp > 100 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s.scale_up must be between 0 and 100, got %v", p.path, p.cfg.ScaleUp)
		}
		if p.cfg.ScaleDown < 0 || p.cfg.ScaleDown > 100 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s.scale_down must be between 0 and 100, got %v", p.path, p.cfg.ScaleDown)
		}
		if p.cfg.ScaleUp > 0 && p.cfg.ScaleDown > 0 && p.cfg.ScaleDown >= p.cfg.ScaleUp {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: %s.scale_down (%v) must be below %s.scale_up (%v)", p.path, p.cfg.ScaleDown, p.path, p.cfg.ScaleUp)
		}
	}
	if m.RequestsPerSecond != nil {
		rps := m.RequestsPerSecond
		if rps.ScaleUp < 0 || rps.ScaleDown < 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.autoscaling.metrics.requests_per_second thresholds must not be negative")
		}
		if rps.ScaleUp > 0 && rps.ScaleDown > 0 && rps.ScaleDown >= rps.ScaleUp {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.autoscaling.metrics.requests_per_second.scale_down (%v) must be below scale_up (%v)", rps.ScaleDown, rps.ScaleUp)
		}
	}
	if m.Latency != nil {
		up, err := parseAutoscaleP95("deploy.autoscaling.metrics.latency.scale_up_p95", m.Latency.ScaleUpP95)
		if err != nil {
			return err
		}
		down, err := parseAutoscaleP95("deploy.autoscaling.metrics.latency.scale_down_p95", m.Latency.ScaleDownP95)
		if err != nil {
			return err
		}
		if up > 0 && down > 0 && down >= up {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.autoscaling.metrics.latency.scale_down_p95 (%s) must be below scale_up_p95 (%s)", m.Latency.ScaleDownP95, m.Latency.ScaleUpP95)
		}
		if m.Latency.ScaleUpP99 != "" || m.Latency.ScaleDownP99 != "" {
			up99, err := parseAutoscaleP95("deploy.autoscaling.metrics.latency.scale_up_p99", m.Latency.ScaleUpP99)
			if err != nil {
				return err
			}
			down99, err := parseAutoscaleP95("deploy.autoscaling.metrics.latency.scale_down_p99", m.Latency.ScaleDownP99)
			if err != nil {
				return err
			}
			if up99 > 0 && down99 > 0 && down99 >= up99 {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: deploy.autoscaling.metrics.latency.scale_down_p99 (%s) must be below scale_up_p99 (%s)", m.Latency.ScaleDownP99, m.Latency.ScaleUpP99)
			}
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
