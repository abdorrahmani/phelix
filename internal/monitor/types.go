package monitor

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// Command types the backend can send over MonitorStream.
const (
	// CommandRollback remotely triggers the existing local rollback engine.
	// Executed through the cmd package's rollback handler (the same service
	// layer the CLI uses), never by shelling out to the CLI binary.
	CommandRollback = "rollback"
)

// MetricsCollector collects the data the monitoring daemon reports to the
// backend. Implementations are transport-agnostic — they know nothing about
// how the data is sent (previously WebSocket, now gRPC).
type MetricsCollector interface {
	CollectAppMetrics() []AppMetrics
	CollectAppDetails() []AppDetails
	CollectServerMetrics() (*server.Metrics, error)
	CollectAppLogs() ([]logs.LogEntry, error)
	CollectSelfLogs() ([]logs.LogEntry, error)
}

// CommandExecutor executes a remote-control command received from the
// backend (e.g. restart/stop a managed application). Transport-agnostic.
type CommandExecutor interface {
	Execute(cmd Command) error
}

// CommandPayload describes a remote command targeting a specific app.
type CommandPayload struct {
	Type    string `json:"type"`
	AppName string `json:"appName"`

	// RequestID is the backend's command correlation id (MonitorCommandRequest
	// request_id). Executors that report async telemetry use it to tag their
	// event stream so the backend can tie events to the originating command.
	RequestID string `json:"requestId,omitempty"`

	// Strategy/Replicas are one-off deployment overrides carried by a
	// "rebuild" command. Empty/zero means the rebuild resolves its strategy
	// the way a local rebuild does — from the app's phelix.yaml — and the
	// override is never written back to that file.
	Strategy string `json:"strategy,omitempty"`
	Replicas int    `json:"replicas,omitempty"`

	// Target/Reason/VerifyDuration/DryRun are remote rollback options carried
	// by a "rollback" command. They mirror the local `phelix rollback` flags:
	// Target uses the same "vN" / "N" / tag vocabulary as --to (empty =
	// previous version), Reason is validated at the rollback boundary,
	// VerifyDuration is a whole observation window in milliseconds, and
	// DryRun routes to the read-only plan path.
	Target         string        `json:"target,omitempty"`
	Reason         string        `json:"reason,omitempty"`
	VerifyDuration time.Duration `json:"verifyDuration,omitempty"`
	DryRun         bool          `json:"dryRun,omitempty"`
}

// Command is a remote-control command received from the backend.
type Command struct {
	Type    string         `json:"type"`
	Payload CommandPayload `json:"payload"`
}

// AppMetrics carries a single application's current CPU/RAM usage.
type AppMetrics struct {
	AppID       string  `json:"appID"`
	ServerID    string  `json:"server_id"`
	CPUUsage    float64 `json:"cpuUsage"`
	MemoryUsage uint64  `json:"memoryUsage"`
}

// AppDetails carries a single application's identity/status/lifecycle info
// plus its reported configuration (process/networking/logging/storage).
type AppDetails struct {
	ID          string           `json:"id"`
	ServerID    string           `json:"server_id"`
	Name        string           `json:"name"`
	Status      string           `json:"status"`
	Language    builder.Language `json:"language"`
	Port        int              `json:"port"`
	BuildStatus string           `json:"buildStatus"`
	PID         int              `json:"pid"`
	Uptime      string           `json:"uptime"`
	CreatedAt   time.Time        `json:"createdAt"`
	UpdatedAt   time.Time        `json:"updatedAt"`

	Process    app.AppProcessConfig    `json:"process,omitempty"`
	Networking app.AppNetworkingConfig `json:"networking,omitempty"`
	Logging    app.AppLoggingConfig    `json:"logging,omitempty"`
	Storage    app.AppStorageConfig    `json:"storage,omitempty"`
}
