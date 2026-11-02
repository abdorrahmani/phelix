package monitor

import (
	"strconv"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/server"
)

// appMetricsCollector Metrics collector implementation
type appMetricsCollector struct{}

// NewMetricsCollector returns the default MetricsCollector implementation,
// backed by the app/server/logs packages. It is transport-agnostic and can
// be reused by any monitoring transport (gRPC today, previously WebSocket).
func NewMetricsCollector() MetricsCollector {
	return &appMetricsCollector{}
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
		status, err := app.Manager.StatusApplication(appInfo.ID)
		if err != nil {
			logs.Error("monitor", "error getting status for app %s: %v", appInfo.Name, err)
			continue
		}

		if summary := workerRuntimeState(appInfo.Name); summary != nil {
			if summary.PID <= 0 {
				continue
			}
			ram, cpu, perr := app.ProcessUsage(summary.PID)
			if perr != nil {
				logs.Error("monitor", "error getting worker process usage for %s: %v", appInfo.Name, perr)
				continue
			}
			metrics = append(metrics, AppMetrics{
				AppID:       appInfo.ID,
				ServerID:    serverID,
				CPUUsage:    cpu,
				MemoryUsage: ram,
			})
			continue
		}

		metrics = append(metrics, AppMetrics{
			AppID:       appInfo.ID,
			ServerID:    serverID,
			CPUUsage:    status.CPUUsage,
			MemoryUsage: status.RAMUsage,
		})
	}
	return metrics
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

// workerRuntimeState returns live worker replica status for a worker app, nil
// for every other app. Reading deploy state is what makes the dashboard follow
// supervisor restarts without waiting for a CLI command to re-sync AppInfo.
func workerRuntimeState(name string) *deploy.WorkerSummary {
	state, err := deploy.Load(name)
	if err != nil || state == nil || state.Mode != deploy.ModeWorker {
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
	// their log tail never reaches the backend. The serving version is
	// resolved best effort so every line is attributable to the deployment
	// that produced it.
	targets := make([]logs.AppLogTarget, 0)
	for _, a := range app.Manager.ListApplications() {
		if !a.Watching {
			continue
		}
		t := a.LogTarget()
		if v, err := deploy.CurrentVersion(a.Name); err == nil && v > 0 {
			t.Version = "v" + strconv.Itoa(v)
		}
		targets = append(targets, t)
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
