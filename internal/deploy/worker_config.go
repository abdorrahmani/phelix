package deploy

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/project"
)

// WorkerConfigState is the resolved supervision configuration of a worker app,
// as declared in its phelix.yaml. It is reported on deployment snapshots so
// the dashboard can show the policy a replica set actually runs under.
type WorkerConfigState struct {
	RestartPolicy      string
	MaxRestartAttempts int
	BackoffInitial     string
	BackoffMax         string
	BackoffMultiplier  float64
	ShutdownSignal     string
	GracePeriod        string
	StartupTimeout     string
	HealthMode         string
	HealthCommand      []string
	HealthInterval     string
	HealthTimeout      string
	HealthRetries      int
	RollingMaxSurge    int
	RollingMaxUnavail  int
	RollingDrain       string
}

// workerConfigFor resolves the worker supervision block of an app from its
// project config. Nil means the app is not a worker, or its project directory
// is unknown to this process, which the wire reports as absent.
func workerConfigFor(appName string) *WorkerConfigState {
	for _, item := range app.Manager.ListApplications() {
		if item.Name != appName {
			continue
		}
		cfg, err := project.Load(item.Directory)
		if err != nil || cfg == nil || !cfg.IsWorker() {
			return nil
		}
		return workerConfigState(cfg)
	}
	return nil
}

// workerConfigState resolves the supervision block of an already-loaded
// worker project config.
func workerConfigState(cfg *project.Config) *WorkerConfigState {
	if cfg == nil || !cfg.IsWorker() {
		return nil
	}
	ps := cfg.Process.Resolve()
	hs := cfg.Health.ResolveWorker()
	var rolling *project.RollingDeployConfig
	if cfg.Deploy != nil {
		rolling = cfg.Deploy.Rolling
	}
	rs := rolling.Resolve()
	return &WorkerConfigState{
		RestartPolicy:      ps.RestartPolicy,
		MaxRestartAttempts: ps.MaxRestartAttempts,
		BackoffInitial:     ps.BackoffInitial.String(),
		BackoffMax:         ps.BackoffMax.String(),
		BackoffMultiplier:  ps.BackoffMultiplier,
		ShutdownSignal:     ps.ShutdownSignal,
		GracePeriod:        ps.GracePeriod.String(),
		StartupTimeout:     ps.StartupTimeout.String(),
		HealthMode:         hs.Mode,
		HealthCommand:      append([]string(nil), hs.Command...),
		HealthInterval:     hs.Interval.String(),
		HealthTimeout:      hs.Timeout.String(),
		HealthRetries:      hs.Retries,
		RollingMaxSurge:    rs.MaxSurge,
		RollingMaxUnavail:  rs.MaxUnavailable,
		RollingDrain:       durationText(rs.DrainTimeout),
	}
}

// durationText renders a duration for the wire, empty when unset.
func durationText(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}
