package grpc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
)

// RollbackReporter emits lifecycle events for every significant step of a
// rollback. The backend receives each event independently, enabling real-time
// progress display, audit trail generation, and failure diagnosis without
// accessing CLI logs.
type RollbackReporter struct {
	event *pb.RollbackLifecycleEvent
}

// RollbackStep constants identify each phase of the rollback lifecycle.
// The backend can use these to reconstruct the full execution timeline,
// display progress bars, and detect where a failure occurred.
const (
	// Pre-flight
	RollbackStepInit         = "init"
	RollbackStepListVersions = "list_versions"

	// Version resolution
	RollbackStepVersionResolving = "version_resolving"
	RollbackStepVersionResolved  = "version_resolved"

	// Lock acquisition (zero-downtime path only)
	RollbackStepLockAcquiring = "lock_acquiring"
	RollbackStepLockAcquired  = "lock_acquired"
	RollbackStepLockFailed    = "lock_failed"

	// State loading (zero-downtime path only)
	RollbackStepStateLoaded = "state_loaded"

	// Proxy (zero-downtime path only)
	RollbackStepProxyEnsuring = "proxy_ensuring"
	RollbackStepProxyReady    = "proxy_ready"

	// Stop current instance (classic path)
	RollbackStepStoppingOld = "stopping_old"
	RollbackStepOldStopped  = "old_stopped"

	// Binary copy (classic path)
	RollbackStepCopyingBinary = "copying_binary"
	RollbackStepBinaryCopied  = "binary_copied"

	// Start new instance (classic path)
	RollbackStepStartingNew = "starting_new"
	RollbackStepNewStarted  = "new_started"

	// Health check (zero-downtime path, inside ExecuteRollback)
	RollbackStepHealthChecking = "health_checking"
	RollbackStepHealthPassed   = "health_passed"

	// Proxy switch (zero-downtime path, inside ExecuteRollback)
	RollbackStepProxySwitching = "proxy_switching"
	RollbackStepProxySwitched  = "proxy_switched"

	// Graceful stop of old instance (zero-downtime path)
	RollbackStepGracefulStopping     = "graceful_stopping"
	RollbackStepOldGracefullyStopped = "old_gracefully_stopped"

	// Version promotion
	RollbackStepPromotingVersion = "promoting_version"
	RollbackStepVersionPromoted  = "version_promoted"

	// Terminal
	RollbackStepComplete = "complete"
	RollbackStepFailed   = "failed"
)

// NewRollbackReporter creates a reporter bound to a specific rollback context.
// Call Emit at each significant step; the reporter handles gRPC dispatch and
// local file logging.
func NewRollbackReporter(serverID, cliAppID, resolvedAppID, appName, mode, strategy, currentVersion, targetVersion, targetTag string) *RollbackReporter {
	hostname, _ := os.Hostname()
	return &RollbackReporter{
		event: &pb.RollbackLifecycleEvent{
			ServerId:         serverID,
			CliAppId:         cliAppID,
			ResolvedAppId:    resolvedAppID,
			AppName:          appName,
			DeploymentMode:   mode,
			RollbackStrategy: strategy,
			CurrentVersion:   currentVersion,
			TargetVersion:    targetVersion,
			TargetTag:        targetTag,
			Pid:              int32(os.Getpid()),
			Hostname:         hostname,
			Metadata:         make(map[string]string),
		},
	}
}

// Emit sends a lifecycle event for the given step. It fills in timestamp,
// duration, success, and message, then dispatches to gRPC and appends to
// the local rollback audit log.
func (r *RollbackReporter) Emit(step string, success bool, msg string, duration time.Duration, errMsg string) {
	r.event.CurrentStep = step
	r.event.Success = success
	r.event.Message = msg
	r.event.Timestamp = time.Now().UnixMilli()
	r.event.DurationMs = duration.Milliseconds()
	r.event.Error = errMsg

	// Ensure server ID is populated.
	if r.event.ServerId == "" {
		_ = server.Initialize()
		r.event.ServerId = server.GetServerID()
	}

	// Attach auth identity to every event so the backend can attribute
	// the rollback to a specific user session without reading headers.
	if sid, tok := loadSessionIdentity(); sid != "" {
		r.event.UserId = sid
		r.event.SessionToken = tok
	}

	// Send to backend.
	sendRollbackLifecycleEvent(r.event)

	// Append to local rollback audit log.
	r.writeLocalLog(step, success, msg, duration, errMsg)
}

// SetVersionList populates the version list for --list events.
func (r *RollbackReporter) SetVersionList(versions []*pb.RollbackVersionEntry) {
	r.event.Versions = versions
}

// SetMetadata adds a key-value pair to the event metadata.
func (r *RollbackReporter) SetMetadata(key, value string) {
	r.event.Metadata[key] = value
}

// Event returns the underlying proto event for inspection or testing.
func (r *RollbackReporter) Event() *pb.RollbackLifecycleEvent {
	return r.event
}

// loadSessionIdentity reads the session ID and token from disk without
// failing the rollback if the session is missing or expired.
func loadSessionIdentity() (sessionID, token string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".phelix", "session.json"))
	if err != nil {
		return "", ""
	}
	var sess struct {
		SessionID string `json:"sessionID"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		return "", ""
	}
	return sess.SessionID, sess.Token
}

// writeLocalLog appends a structured line to ~/.phelix/apps/<app>/rollback.log.
func (r *RollbackReporter) writeLocalLog(step string, success bool, msg string, duration time.Duration, errMsg string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	logPath := filepath.Join(home, ".phelix", "apps", r.event.AppName, "rollback.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	status := "ok"
	if !success {
		status = "FAILED"
		if errMsg != "" {
			status += ": " + errMsg
		}
	}

	if msg != "" {
		line := fmt.Sprintf("%s [%s] %s -> %s step=%s dur=%s msg=%q %s\n",
			time.Now().Format(time.RFC3339),
			r.event.AppName,
			r.event.CurrentVersion,
			r.event.TargetVersion,
			step,
			duration.Round(time.Millisecond),
			msg,
			status,
		)
		_, _ = f.WriteString(line)
	} else {
		line := fmt.Sprintf("%s [%s] %s -> %s step=%s dur=%s %s\n",
			time.Now().Format(time.RFC3339),
			r.event.AppName,
			r.event.CurrentVersion,
			r.event.TargetVersion,
			step,
			duration.Round(time.Millisecond),
			status,
		)
		_, _ = f.WriteString(line)
	}
}

// sendRollbackLifecycleEvent dispatches the event to the backend via gRPC.
func sendRollbackLifecycleEvent(event *pb.RollbackLifecycleEvent) {
	c := GetClient()
	if c != nil && c.IsConnected() {
		c.SendRollbackEvent(event)
		return
	}
	// No global client — create a temporary connection.
	sendRollbackEventWithTemporaryClient(event)
}

func sendRollbackEventWithTemporaryClient(event *pb.RollbackLifecycleEvent) {
	grpcLog("[gRPC] Creating temporary connection to send rollback event: step=%s", event.GetCurrentStep())
	c := NewClient()
	if err := c.Connect(); err != nil {
		grpcLog("[gRPC] Failed to create temporary connection for rollback event: %v", err)
		return
	}
	defer c.Close()
	c.SendRollbackEvent(event)
}
