package monitor

import (
	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/logs"
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
		apps = append(apps, AppDetails{
			ID:          appInfo.ID,
			ServerID:    serverID,
			Name:        appInfo.Name,
			Status:      appInfo.Status,
			BuildStatus: appInfo.BuildStatus,
			Language:    builder.Language(appInfo.Language),
			PID:         appInfo.PID,
			Port:        appInfo.Port,
			Uptime:      appInfo.Uptime,
			CreatedAt:   appInfo.CreatedAt,
			UpdatedAt:   appInfo.UpdatedAt,
			Process:     appInfo.Process,
			Networking:  appInfo.Networking,
			Logging:     appInfo.Logging,
			Storage:     appInfo.Storage,
		})
	}
	return apps
}

func (c *appMetricsCollector) CollectServerMetrics() (*server.Metrics, error) {
	return server.CollectMetrics()
}

func (c *appMetricsCollector) CollectAppLogs() ([]logs.LogEntry, error) {
	// Resolve the app manager's registered apps into plain log targets. The
	// logs package itself stays app-agnostic. Unwatched apps are excluded so
	// their log tail never reaches the backend.
	targets := make([]logs.AppLogTarget, 0)
	for _, a := range app.Manager.ListApplications() {
		if !a.Watching {
			continue
		}
		targets = append(targets, logs.AppLogTarget{ID: a.ID})
	}
	return logs.CollectAppLogs(targets, server.GetServerID())
}

func (c *appMetricsCollector) CollectSelfLogs() ([]logs.LogEntry, error) {
	return logs.CollectSelfLogs(server.GetServerID())
}
