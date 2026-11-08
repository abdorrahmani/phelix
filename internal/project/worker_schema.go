package project

import (
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Workload types for the top-level `type` key. An absent key means
// TypeService: every project written before the key existed is a service.
const (
	TypeService = "service"
	TypeWorker  = "worker"
)

// Restart policies for process.restart.policy.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNever     = "never"
)

// Health modes for a worker's health block. `process` watches the process
// identity; `exec` runs a command and treats exit 0 as healthy. The endpoint
// modes (auto/http/tcp-only/none) are port-based and rejected for workers.
const (
	WorkerHealthProcess = "process"
	WorkerHealthExec    = "exec"
)

// Shutdown signals accepted for process.shutdown.signal.
const (
	ShutdownSIGTERM = "SIGTERM"
	ShutdownSIGINT  = "SIGINT"
)

// Defaults applied by the worker Resolve methods for fields left unset.
const (
	DefaultRestartPolicy       = RestartOnFailure
	DefaultMaxRestartAttempts  = 10
	DefaultBackoffInitial      = 2 * time.Second
	DefaultBackoffMax          = 60 * time.Second
	DefaultBackoffMultiplier   = 2.0
	DefaultShutdownSignal      = ShutdownSIGTERM
	DefaultShutdownGracePeriod = 30 * time.Second
	DefaultStartupTimeout      = 30 * time.Second
	DefaultWorkerHealthMode    = WorkerHealthProcess
	DefaultWorkerExecInterval  = 10 * time.Second
	DefaultWorkerExecTimeout   = 5 * time.Second
	DefaultWorkerExecRetries   = 3
)

// StableWindow is how long a replica must stay up before its consecutive
// restart counter resets. shortcut: fixed in v1, expose it as
// process.restart.stable_window when a real need appears.
const StableWindow = 300 * time.Second

var supportedTypes = map[string]bool{
	TypeService: true, TypeWorker: true,
}

var supportedRestartPolicies = map[string]bool{
	RestartAlways: true, RestartOnFailure: true, RestartNever: true,
}

var supportedShutdownSignals = map[string]bool{
	ShutdownSIGTERM: true, ShutdownSIGINT: true,
}

var supportedWorkerHealthModes = map[string]bool{
	WorkerHealthProcess: true, WorkerHealthExec: true,
}

// ProcessConfig is the top-level `process` block: supervision policy for a
// worker's replicas. It is rejected for services, whose lifecycle is owned by
// the deploy strategy and the health daemon.
type ProcessConfig struct {
	Restart        *RestartConfig  `yaml:"restart,omitempty"`
	Shutdown       *ShutdownConfig `yaml:"shutdown,omitempty"`
	StartupTimeout string          `yaml:"startup_timeout,omitempty"`
}

// RestartConfig controls crash handling: when a dead replica is relaunched,
// how fast, and when Phelix gives up (crashloop).
type RestartConfig struct {
	Policy      string         `yaml:"policy,omitempty"`
	MaxAttempts int            `yaml:"max_attempts,omitempty"`
	Backoff     *BackoffConfig `yaml:"backoff,omitempty"`
}

// BackoffConfig shapes the restart delay: initial * multiplier^attempt,
// capped at max. The supervisor applies jitter on top.
type BackoffConfig struct {
	Initial    string  `yaml:"initial,omitempty"`
	Max        string  `yaml:"max,omitempty"`
	Multiplier float64 `yaml:"multiplier,omitempty"`
}

// ShutdownConfig controls graceful shutdown: which signal to send and how
// long to wait before SIGKILL.
type ShutdownConfig struct {
	Signal      string `yaml:"signal,omitempty"`
	GracePeriod string `yaml:"grace_period,omitempty"`
}

// RollingDeployConfig holds the worker rolling-replacement parameters. v1
// implements the sequential policy (surge 1, unavailable 0); other values are
// rejected at validation so a file never promises behavior the engine does
// not have.
type RollingDeployConfig struct {
	MaxSurge       int    `yaml:"max_surge,omitempty"`
	MaxUnavailable int    `yaml:"max_unavailable,omitempty"`
	DrainTimeout   string `yaml:"drain_timeout,omitempty"`
}

// RollingSettings is RollingDeployConfig resolved.
type RollingSettings struct {
	MaxSurge       int
	MaxUnavailable int
	DrainTimeout   time.Duration
}

// Resolve applies defaults. A zero DrainTimeout means "fall back to the
// shutdown grace period" at the call site. Call only on a config that passed
// validate.
func (r *RollingDeployConfig) Resolve() RollingSettings {
	if r == nil {
		return RollingSettings{MaxSurge: 1}
	}
	s := RollingSettings{MaxSurge: r.MaxSurge, MaxUnavailable: r.MaxUnavailable}
	if s.MaxSurge < 1 {
		s.MaxSurge = 1
	}
	if s.MaxUnavailable < 0 {
		s.MaxUnavailable = 0
	}
	if r.DrainTimeout != "" {
		if d, err := time.ParseDuration(r.DrainTimeout); err == nil && d > 0 {
			s.DrainTimeout = d
		}
	}
	return s
}

// ExecHealthConfig is the worker exec health probe. A probe that exits 0
// within Timeout counts as healthy; retries consecutive failures mark the
// replica unhealthy.
type ExecHealthConfig struct {
	Command  []string `yaml:"command"`
	Interval string   `yaml:"interval,omitempty"`
	Timeout  string   `yaml:"timeout,omitempty"`
	Retries  int      `yaml:"retries,omitempty"`
}

// WorkerHealthSettings is the worker health block resolved: defaults applied,
// durations parsed.
type WorkerHealthSettings struct {
	Mode     string
	Command  []string
	Interval time.Duration
	Timeout  time.Duration
	Retries  int
}

// ResolveWorker resolves the worker-relevant fields of the health block. Call
// only on a config that passed validate.
func (h *HealthConfig) ResolveWorker() WorkerHealthSettings {
	s := WorkerHealthSettings{
		Mode:     DefaultWorkerHealthMode,
		Interval: DefaultWorkerExecInterval,
		Timeout:  DefaultWorkerExecTimeout,
		Retries:  DefaultWorkerExecRetries,
	}
	if h == nil {
		return s
	}
	if h.Mode != "" {
		s.Mode = h.Mode
	}
	if h.Exec != nil {
		s.Command = h.Exec.Command
		if d, err := time.ParseDuration(h.Exec.Interval); err == nil && d > 0 {
			s.Interval = d
		}
		if d, err := time.ParseDuration(h.Exec.Timeout); err == nil && d > 0 {
			s.Timeout = d
		}
		if h.Exec.Retries > 0 {
			s.Retries = h.Exec.Retries
		}
	}
	return s
}

// ProcessSettings is the process block fully resolved.
type ProcessSettings struct {
	RestartPolicy      string
	MaxRestartAttempts int
	BackoffInitial     time.Duration
	BackoffMax         time.Duration
	BackoffMultiplier  float64
	ShutdownSignal     string
	GracePeriod        time.Duration
	StartupTimeout     time.Duration
}

// Resolve applies the documented defaults. Call only on a config that passed
// validate: unparseable values fall back to defaults here.
func (p *ProcessConfig) Resolve() ProcessSettings {
	s := ProcessSettings{
		RestartPolicy:      DefaultRestartPolicy,
		MaxRestartAttempts: DefaultMaxRestartAttempts,
		BackoffInitial:     DefaultBackoffInitial,
		BackoffMax:         DefaultBackoffMax,
		BackoffMultiplier:  DefaultBackoffMultiplier,
		ShutdownSignal:     DefaultShutdownSignal,
		GracePeriod:        DefaultShutdownGracePeriod,
		StartupTimeout:     DefaultStartupTimeout,
	}
	if p == nil {
		return s
	}
	if p.StartupTimeout != "" {
		if d, err := time.ParseDuration(p.StartupTimeout); err == nil && d > 0 {
			s.StartupTimeout = d
		}
	}
	if p.Shutdown != nil {
		if p.Shutdown.Signal != "" {
			s.ShutdownSignal = p.Shutdown.Signal
		}
		if d, err := time.ParseDuration(p.Shutdown.GracePeriod); err == nil && d > 0 {
			s.GracePeriod = d
		}
	}
	if p.Restart != nil {
		if p.Restart.Policy != "" {
			s.RestartPolicy = p.Restart.Policy
		}
		if p.Restart.MaxAttempts > 0 {
			s.MaxRestartAttempts = p.Restart.MaxAttempts
		}
		if b := p.Restart.Backoff; b != nil {
			if d, err := time.ParseDuration(b.Initial); err == nil && d > 0 {
				s.BackoffInitial = d
			}
			if d, err := time.ParseDuration(b.Max); err == nil && d > 0 {
				s.BackoffMax = d
			}
			if b.Multiplier >= 1 {
				s.BackoffMultiplier = b.Multiplier
			}
		}
	}
	return s
}

// WorkloadType returns the declared workload type, defaulting to TypeService
// for an absent key or a nil Config.
func (c *Config) WorkloadType() string {
	if c == nil {
		return TypeService
	}
	if t := strings.TrimSpace(c.Type); t != "" {
		return t
	}
	return TypeService
}

// IsWorker reports whether this project declares type: worker.
func (c *Config) IsWorker() bool {
	return c.WorkloadType() == TypeWorker
}

// validateType checks the top-level type key itself.
func (c *Config) validateType() error {
	t := strings.TrimSpace(c.Type)
	if t != "" && !supportedTypes[t] {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: invalid type %q\nHint: expected one of: service, worker", c.Type)
	}
	return nil
}

// validateWorker checks the worker-only constraints on top of the shared
// deploy/health validation: workers have no port, no endpoint probes, no
// canary machinery, and no docker runtime in v1.
func (c *Config) validateWorker() error {
	if c.Port != 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: port is not valid for type: worker\nHint: workers do not listen on a port; remove the top-level port key")
	}
	if c.Deploy != nil {
		s := strings.TrimSpace(c.Deploy.Strategy)
		switch s {
		case "", StrategyClassic, StrategyRolling:
		default:
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.strategy %q is not valid for type: worker\nHint: workers support classic or rolling", c.Deploy.Strategy)
		}
		if strings.TrimSpace(c.Deploy.Runtime) == RuntimeDocker {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.runtime: docker is not supported for type: worker\nHint: workers run as supervised host processes (native runtime)")
		}
		if c.Deploy.Rollout != nil {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.rollout is not valid for type: worker\nHint: canary/progressive rollouts route proxy traffic; workers have none")
		}
		if err := c.Deploy.Rolling.validate(); err != nil {
			return err
		}
	}
	if err := c.Health.validateWorkerHealth(); err != nil {
		return err
	}
	return c.Process.validate()
}

// validateWorkerHealth checks the worker health block: mode, and the exec
// probe shape when mode is exec. Endpoint probes are port-based and rejected.
func (h *HealthConfig) validateWorkerHealth() error {
	if h == nil {
		return nil
	}
	if len(h.Endpoints) > 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: health.endpoints is not valid for type: worker\nHint: workers have no port; use health.mode: process or exec")
	}
	mode := strings.TrimSpace(h.Mode)
	switch mode {
	case "", WorkerHealthProcess, WorkerHealthExec:
	default:
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: invalid health.mode %q\nHint: workers support process or exec", h.Mode)
	}
	if mode != WorkerHealthExec && h.Exec != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: health.exec requires health.mode: exec")
	}
	if mode == WorkerHealthExec && h.Exec == nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: health.mode: exec requires a health.exec block\nHint: configure the probe argv, e.g. command: [\"./healthcheck\"]")
	}
	if h.Exec == nil {
		return nil
	}
	if len(h.Exec.Command) == 0 || strings.TrimSpace(h.Exec.Command[0]) == "" {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: health.exec.command is required\nHint: list the probe argv, e.g. command: [\"./healthcheck\"]")
	}
	if err := validateStepDuration("health.exec.interval", h.Exec.Interval); err != nil {
		return err
	}
	if err := validateStepDuration("health.exec.timeout", h.Exec.Timeout); err != nil {
		return err
	}
	if h.Exec.Retries < 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: health.exec.retries must be >= 0, got %d", h.Exec.Retries)
	}
	return nil
}

// validate checks the worker rolling parameters. v1 runs the sequential
// replacement (one replacement in flight, no availability loss), so other
// surge/unavailable values are rejected instead of silently ignored.
func (r *RollingDeployConfig) validate() error {
	if r == nil {
		return nil
	}
	if r.MaxSurge != 0 && r.MaxSurge != 1 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: deploy.rolling.max_surge must be 1 for now, got %d\nHint: v1 replaces replicas one at a time; parallel surge is a planned extension", r.MaxSurge)
	}
	if r.MaxUnavailable != 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: deploy.rolling.max_unavailable must be 0 for now, got %d\nHint: v1 never takes a healthy replica out before its replacement is ready", r.MaxUnavailable)
	}
	return validateStepDuration("deploy.rolling.drain_timeout", r.DrainTimeout)
}

// validate checks the process block: policy vocabulary, attempt and backoff
// bounds, shutdown signal and durations.
func (p *ProcessConfig) validate() error {
	if p == nil {
		return nil
	}
	if p.Restart != nil {
		if pol := strings.TrimSpace(p.Restart.Policy); pol != "" && !supportedRestartPolicies[pol] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid process.restart.policy %q\nHint: expected one of: always, on-failure, never", p.Restart.Policy)
		}
		if p.Restart.MaxAttempts < 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: process.restart.max_attempts must be >= 1, got %d", p.Restart.MaxAttempts)
		}
		if b := p.Restart.Backoff; b != nil {
			if b.Initial != "" {
				if d, err := time.ParseDuration(b.Initial); err != nil || d <= 0 {
					return phelixerr.Newf(phelixerr.CodeConfiguration,
						"configuration error: process.restart.backoff.initial must be a positive duration like 2s, got %q", b.Initial)
				}
			}
			if b.Max != "" {
				if d, err := time.ParseDuration(b.Max); err != nil || d <= 0 {
					return phelixerr.Newf(phelixerr.CodeConfiguration,
						"configuration error: process.restart.backoff.max must be a positive duration like 60s, got %q", b.Max)
				}
			}
			if b.Multiplier != 0 && b.Multiplier < 1 {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: process.restart.backoff.multiplier must be >= 1, got %v", b.Multiplier)
			}
		}
	}
	if p.Shutdown != nil {
		if sig := strings.TrimSpace(p.Shutdown.Signal); sig != "" && !supportedShutdownSignals[sig] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid process.shutdown.signal %q\nHint: expected SIGTERM or SIGINT", p.Shutdown.Signal)
		}
		if err := validateStepDuration("process.shutdown.grace_period", p.Shutdown.GracePeriod); err != nil {
			return err
		}
	}
	return validateStepDuration("process.startup_timeout", p.StartupTimeout)
}

// validateServiceRejectsWorkerKeys keeps service files honest: the worker
// supervision keys are rejected loudly instead of being parsed and ignored.
func (c *Config) validateServiceRejectsWorkerKeys() error {
	if c.Process != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: process is only valid for type: worker\nHint: add type: worker, or remove the process block")
	}
	if c.Health != nil {
		if c.Health.Mode != "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.mode is only valid for type: worker\nHint: services configure probes via health.endpoints")
		}
		if c.Health.Exec != nil {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.exec is only valid for type: worker")
		}
	}
	if c.Deploy != nil && c.Deploy.Rolling != nil {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: deploy.rolling is only valid for type: worker\nHint: service rolling replicas are tuned via deploy.replicas")
	}
	return nil
}
