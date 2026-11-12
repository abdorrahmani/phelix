package monitor

import (
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/autoscale"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/secrets"
	"github.com/abdorrahmani/phelix/internal/server"
)

// appMetricsCollector Metrics collector implementation. samplers keeps one
// per-app sampler alive so CPU deltas survive between monitoring ticks; the
// mutex guards that map only.
type appMetricsCollector struct {
	mu       sync.Mutex
	samplers map[string]*autoscale.Sampler
}

// NewMetricsCollector returns the default MetricsCollector implementation,
// backed by the app/server/logs packages. It is transport-agnostic and can
// be reused by any monitoring transport (gRPC today, previously WebSocket).
func NewMetricsCollector() MetricsCollector {
	return &appMetricsCollector{samplers: make(map[string]*autoscale.Sampler)}
}

func (c *appMetricsCollector) CollectAppMetrics() []AppMetrics {
	var metrics []AppMetrics
	apps := app.Manager.ListApplications()
	serverID := server.GetServerID()

	for _, appInfo := range apps {
		// Unwatched apps are excluded at the collection boundary: the backend
		// receives no monitoring data for them, not an empty payload.
		if !appInfo.Watching {
			continue
		}

		if state := workerState(appInfo.Name); state != nil {
			pids := deploy.WorkerAlivePIDs(state)
			if len(pids) == 0 {
				continue
			}
			var ram uint64
			var cpu float64
			for _, pid := range pids {
				r, c, perr := app.ProcessUsage(pid)
				if perr != nil {
					logs.Error("monitor", "error getting worker process usage for %s: %v", appInfo.Name, perr)
					continue
				}
				ram += r
				cpu += c
			}
			metrics = append(metrics, AppMetrics{
				AppID:       appInfo.ID,
				AppName:     appInfo.Name,
				ServerID:    serverID,
				CPUUsage:    cpu,
				MemoryUsage: ram,
			})
			continue
		}

		status, err := app.Manager.StatusApplication(appInfo.ID)
		if err != nil {
			logs.Error("monitor", "error getting status for app %s: %v", appInfo.Name, err)
			continue
		}

		metrics = append(metrics, AppMetrics{
			AppID:       appInfo.ID,
			AppName:     appInfo.Name,
			ServerID:    serverID,
			CPUUsage:    status.CPUUsage,
			MemoryUsage: status.RAMUsage,
		})
	}
	return metrics
}

// CollectReplicaMetrics samples every watched app's recorded instances. It
// uses the same sampler as autoscaling, so the CPU definition and the memory
// limit resolution are identical; it never depends on autoscaling being
// enabled.
func (c *appMetricsCollector) CollectReplicaMetrics() []ReplicaMetricsSnapshot {
	var snapshots []ReplicaMetricsSnapshot
	serverID := server.GetServerID()
	for _, appInfo := range app.Manager.ListApplications() {
		if !appInfo.Watching {
			continue
		}
		state, err := deploy.Load(appInfo.Name)
		if err != nil || state == nil {
			continue
		}
		refs := autoscale.ResourceRefs(state)
		if len(refs) == 0 {
			continue
		}
		limit, _ := appInfo.Resources.Limits()
		sampler := c.samplerFor(appInfo.Name)
		sampler.SetMemoryLimit(limit.MemoryBytes)
		sampler.SetFreshnessWindow(autoscale.DefaultFreshnessWindow)
		samples := sampler.CollectResources(refs)
		replicas := make([]ReplicaMetrics, 0, len(samples))
		for _, s := range samples {
			replicas = append(replicas, ReplicaMetrics{
				Replica:          s.Key,
				PID:              s.PID,
				ProcessStatus:    s.ProcessStatus,
				CPUPercent:       s.CPUPercent,
				CPUState:         s.CPUState,
				MemoryBytes:      s.MemoryBytes,
				MemoryState:      s.MemoryState,
				MemoryLimitBytes: s.MemoryLimit,
				MemoryPercent:    s.MemoryPercent,
				MemoryPctState:   s.MemoryPctState,
				HostMemoryPct:    s.HostMemoryPct,
				HostMemState:     s.HostMemState,
				CollectedAt:      s.At,
				Error:            s.Error,
			})
		}
		snapshots = append(snapshots, ReplicaMetricsSnapshot{
			AppID:       appInfo.ID,
			AppName:     appInfo.Name,
			ServerID:    serverID,
			Replicas:    replicas,
			CollectedAt: samples[0].At,
		})
	}
	return snapshots
}

func (c *appMetricsCollector) samplerFor(appName string) *autoscale.Sampler {
	c.mu.Lock()
	defer c.mu.Unlock()
	sampler := c.samplers[appName]
	if sampler == nil {
		sampler = autoscale.NewSampler(appName, nil)
		c.samplers[appName] = sampler
	}
	return sampler
}

func (c *appMetricsCollector) CollectAppDetails() []AppDetails {
	var apps []AppDetails
	appList := app.Manager.ListApplications()
	serverID := server.GetServerID()

	for _, appInfo := range appList {
		// Same watching gate as CollectAppMetrics: app details are dashboard
		// monitoring data.
		if !appInfo.Watching {
			continue
		}
		detail := AppDetails{
			ID:           appInfo.ID,
			ServerID:     serverID,
			Name:         appInfo.Name,
			Status:       appInfo.Status,
			BuildStatus:  appInfo.BuildStatus,
			Language:     builder.Language(appInfo.Language),
			PID:          appInfo.PID,
			Port:         appInfo.Port,
			Uptime:       appInfo.Uptime,
			CreatedAt:    appInfo.CreatedAt,
			UpdatedAt:    appInfo.UpdatedAt,
			Process:      appInfo.Process,
			Networking:   appInfo.Networking,
			Logging:      appInfo.Logging,
			Storage:      appInfo.Storage,
			WorkloadType: workloadTypeOf(appInfo),
		}
		if summary := workerRuntimeState(appInfo.Name); summary != nil {
			detail.WorkloadType = project.TypeWorker
			detail.Status = summary.Status
			detail.PID = summary.PID
			detail.Port = 0
			if summary.PID > 0 && !summary.StartedAt.IsZero() {
				detail.Uptime = app.FormatDuration(time.Since(summary.StartedAt))
			} else {
				detail.Uptime = "N/A"
			}
		}
		apps = append(apps, detail)
	}
	return apps
}

// workloadTypeOf resolves an app's declared workload type. A missing or
// unreadable project config keeps the service default, which is the same
// meaning an absent type key has.
func workloadTypeOf(info app.AppListItem) string {
	cfg, err := project.Load(info.Directory)
	if err != nil || cfg == nil {
		return project.TypeService
	}
	return cfg.WorkloadType()
}

// workerState loads a worker app's deploy state, nil for every other app.
func workerState(name string) *deploy.DeployState {
	state, err := deploy.Load(name)
	if err != nil || state == nil || state.Mode != deploy.ModeWorker {
		return nil
	}
	return state
}

// workerRuntimeState returns live worker replica status for a worker app, nil
// for every other app. Reading deploy state is what makes the dashboard follow
// supervisor restarts without waiting for a CLI command to re-sync AppInfo.
func workerRuntimeState(name string) *deploy.WorkerSummary {
	state := workerState(name)
	if state == nil {
		return nil
	}
	summary := deploy.SummarizeWorker(state)
	return &summary
}

func (c *appMetricsCollector) CollectServerMetrics() (*server.Metrics, error) {
	return server.CollectMetrics()
}

func (c *appMetricsCollector) CollectAppLogs() ([]logs.LogEntry, error) {
	// Resolve the app manager's registered apps into plain log targets. The
	// logs package itself stays app-agnostic. Unwatched apps are excluded so
	// their log tail never reaches the backend. Worker apps expand into one
	// target per replica, each carrying its slot identifier.
	targets := make([]logs.AppLogTarget, 0)
	for _, a := range app.Manager.ListApplications() {
		if !a.Watching {
			continue
		}
		if err := deploy.RefreshSecretCoverage(a); err != nil {
			continue
		}
		if state, _ := secrets.SecurityOf(a.ID); state == secrets.SecurityDegraded {
			continue
		}
		targets = append(targets, AppLogTargets(a)...)
	}
	return logs.CollectAppLogs(targets, server.GetServerID())
}

// CollectAccessLogs tails the proxy daemon's sampled access log; the entries
// ship to the backend inside the log batch only.
func (c *appMetricsCollector) CollectAccessLogs() ([]logs.LogEntry, error) {
	return logs.CollectAccessLogs(server.GetServerID())
}

func (c *appMetricsCollector) CollectSelfLogs() ([]logs.LogEntry, error) {
	return logs.CollectSelfLogs(server.GetServerID())
}
