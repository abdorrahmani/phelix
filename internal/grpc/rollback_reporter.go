package grpc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/abdorrahmani/phelix/internal/deploy"
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
	// RollbackStepInit Pre-flight
	RollbackStepInit         = "init"
	RollbackStepListVersions = "list_versions"

	// RollbackStepVersionResolved RollbackStepVersionResolving Version resolution
	RollbackStepVersionResolved = "version_resolved"

	// RollbackStepProxyEnsuring Proxy (zero-downtime path only)
	RollbackStepProxyEnsuring = "proxy_ensuring"
	RollbackStepProxyReady    = "proxy_ready"

	// RollbackStepOldStopped RollbackStepStoppingOld Stop current instance (classic path)
	RollbackStepOldStopped = "old_stopped"

	// RollbackStepBinaryCopied RollbackStepCopyingBinary Binary copy (classic path)
	RollbackStepBinaryCopied = "binary_copied"

	// RollbackStepNewStarted RollbackStepStartingNew Start new instance (classic path)
	RollbackStepNewStarted = "new_started"

	// RollbackStepVersionPromoted Version promotion
	RollbackStepVersionPromoted = "version_promoted"

	// RollbackStepComplete Terminal
	RollbackStepComplete = "complete"
	RollbackStepFailed   = "failed"
)

// ---------------------------------------------------------------------------
// Background event sender
// ---------------------------------------------------------------------------

var (
	rollbackEventCh    chan *pb.RollbackLifecycleEvent
	rollbackEventOnce  sync.Once
	rollbackSenderDone chan struct{}
	rollbackSenderMu   sync.Mutex // protects init + flush
)

// initRollbackSender starts a single background goroutine that drains the
// rollback event channel and sends each event via gRPC. It is started once
// and lives for the lifetime of the process.
func initRollbackSender() {
	rollbackSenderMu.Lock()
	defer rollbackSenderMu.Unlock()

	rollbackEventOnce.Do(func() {
		rollbackEventCh = make(chan *pb.RollbackLifecycleEvent, 64)
		rollbackSenderDone = make(chan struct{})
		go rollbackSenderLoop()
		grpcLog("[gRPC] Rollback sender: background worker started")
	})
}

// rollbackSenderLoop reads events from the channel and sends them. It reuses
// a single temporary connection for a batch of events to avoid the overhead of
// connect/close per event.
func rollbackSenderLoop() {
	defer close(rollbackSenderDone)

	var (
		cachedClient *Client
		cachedMu     sync.Mutex
		sentCount    int
		failCount    int
	)

	// ensureClient returns a connected client, creating one if needed.
	ensureClient := func() *Client {
		cachedMu.Lock()
		defer cachedMu.Unlock()

		if cachedClient != nil && cachedClient.IsConnected() {
			return cachedClient
		}
		// Close stale client.
		if cachedClient != nil {
			grpcLog("[gRPC] Rollback sender: closing stale connection")
			cachedClient.Close()
			cachedClient = nil
		}

		c := NewClient()
		if err := c.Connect(); err != nil {
			grpcLog("[gRPC] Rollback sender: failed to connect: %v", err)
			return nil
		}
		cachedClient = c
		grpcLog("[gRPC] Rollback sender: connected to backend")
		return cachedClient
	}

	// cleanup closes the cached connection when the channel is drained.
	cleanup := func() {
		cachedMu.Lock()
		if cachedClient != nil {
			grpcLog("[gRPC] Rollback sender: closing connection after draining")
			cachedClient.Close()
			cachedClient = nil
		}
		cachedMu.Unlock()
	}

	grpcLog("[gRPC] Rollback sender: waiting for events...")

	for event := range rollbackEventCh {
		sanitizeEventStrings(event)

		step := event.GetCurrentStep()
		app := event.GetAppName()
		grpcLog("[gRPC] Rollback sender: processing event step=%s app=%s", step, app)

		c := ensureClient()
		if c == nil {
			failCount++
			grpcLog("[gRPC] Rollback sender: NO CONNECTION — dropping event step=%s app=%s (failures=%d)", step, app, failCount)
			continue
		}

		// Try full event first.
		if c.SendRollbackEvent(event) {
			sentCount++
			grpcLog("[gRPC] Rollback sender: sent step=%s app=%s (sent=%d)", step, app, sentCount)
			continue
		}

		failCount++
		grpcLog("[gRPC] Rollback sender: SEND FAILED step=%s app=%s (failures=%d)", step, app, failCount)
		cachedMu.Lock()
		if cachedClient != nil {
			cachedClient.Close()
			cachedClient = nil
		}
		cachedMu.Unlock()
	}

	cleanup()
	grpcLog("[gRPC] Rollback sender: stopped (sent=%d, failed=%d)", sentCount, failCount)
}

// StopRollbackSender drains the event channel and waits for the background
// goroutine to finish. It returns after all queued events have been sent or
// the timeout expires. Safe to call even if no sender was started.
func StopRollbackSender(timeout time.Duration) {
	rollbackSenderMu.Lock()
	ch := rollbackEventCh
	done := rollbackSenderDone
	rollbackSenderMu.Unlock()

	if ch == nil || done == nil {
		grpcLog("[gRPC] Rollback sender: no sender to stop")
		return
	}

	// Close the channel so the loop exits after draining.
	close(ch)
	rollbackEventCh = nil
	rollbackSenderDone = nil

	// Reset the Once so a future rollback can start a new sender.
	rollbackEventOnce = sync.Once{}

	grpcLog("[gRPC] Rollback sender: flushing (timeout=%s)...", timeout)

	select {
	case <-done:
		grpcLog("[gRPC] Rollback sender: flush complete")
	case <-time.After(timeout):
		grpcLog("[gRPC] Rollback sender: flush timed out after %s", timeout)
	}
}

// enqueueRollbackEvent sends an event to the background sender. Non-blocking:
// if the buffer is full the event is dropped (the local log still records
// grpc=dropped).
func enqueueRollbackEvent(event *pb.RollbackLifecycleEvent) bool {
	initRollbackSender()

	// Deep-copy the event so the caller can safely mutate its proto after return.
	cp := cloneRollbackEvent(event)

	select {
	case rollbackEventCh <- cp:
		grpcLog("[gRPC] Rollback event queued: step=%s app=%s", event.GetCurrentStep(), event.GetAppName())
		return true
	default:
		grpcLog("[gRPC] Rollback event channel full, DROPPED step=%s app=%s", event.GetCurrentStep(), event.GetAppName())
		return false
	}
}

// cloneRollbackEvent performs a deep copy of the proto so the background
// sender can safely read it after the caller mutates the original.
func cloneRollbackEvent(e *pb.RollbackLifecycleEvent) *pb.RollbackLifecycleEvent {
	cp := *e
	// Deep-copy the metadata map so the caller can keep mutating it.
	if e.Metadata != nil {
		cp.Metadata = make(map[string]string, len(e.Metadata))
		for k, v := range e.Metadata {
			cp.Metadata[k] = v
		}
	}
	// Deep-copy the versions slice.
	if e.Versions != nil {
		cp.Versions = make([]*pb.RollbackVersionEntry, len(e.Versions))
		for i, v := range e.Versions {
			cv := *v
			cp.Versions[i] = &cv
		}
	}
	return &cp
}

// ---------------------------------------------------------------------------
// UTF-8 sanitisation
// ---------------------------------------------------------------------------

// sanitizeEventStrings replaces any invalid UTF-8 bytes in every string field
// of the event with the Unicode replacement character (U+FFFD). This prevents
// the backend from rejecting the request with "string field contains invalid
// UTF-8" or "cannot parse invalid wire-format data".
func sanitizeEventStrings(e *pb.RollbackLifecycleEvent) {
	e.ServerId = sanitizeUTF8(e.ServerId)
	e.CliAppId = sanitizeUTF8(e.CliAppId)
	e.ResolvedAppId = sanitizeUTF8(e.ResolvedAppId)
	e.AppName = sanitizeUTF8(e.AppName)
	e.DeploymentMode = sanitizeUTF8(e.DeploymentMode)
	e.RollbackStrategy = sanitizeUTF8(e.RollbackStrategy)
	e.CurrentVersion = sanitizeUTF8(e.CurrentVersion)
	e.TargetVersion = sanitizeUTF8(e.TargetVersion)
	e.TargetTag = sanitizeUTF8(e.TargetTag)
	e.CurrentStep = sanitizeUTF8(e.CurrentStep)
	e.Message = sanitizeUTF8(e.Message)
	e.Error = sanitizeUTF8(e.Error)
	e.UserId = sanitizeUTF8(e.UserId)
	e.SessionToken = sanitizeUTF8(e.SessionToken)

	for k, v := range e.Metadata {
		e.Metadata[k] = sanitizeUTF8(v)
	}

	for _, v := range e.Versions {
		v.Tag = sanitizeUTF8(v.Tag)
		v.GitCommit = sanitizeUTF8(v.GitCommit)
	}
}

// sanitizeUTF8 returns s if it is already valid UTF-8. Otherwise it returns a
// copy with every invalid byte replaced by U+FFFD.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	b := []byte(s)
	var buf []byte
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			buf = append(buf, []byte(string(utf8.RuneError))...)
		} else {
			buf = append(buf, string(r)...)
		}
		b = b[size:]
	}
	return string(buf)
}

// ---------------------------------------------------------------------------
// Reporter
// ---------------------------------------------------------------------------

// NewRollbackReporter creates a reporter bound to a specific rollback context.
// Call Emit at each significant step; the reporter handles gRPC dispatch and
// local file logging.
func NewRollbackReporter(serverID, cliAppID, resolvedAppID, appName, mode, strategy, currentVersion, targetVersion, targetTag string) *RollbackReporter {
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
			Metadata:         make(map[string]string),
		},
	}
}

// Emit sends a lifecycle event for the given step. It fills in timestamp,
// duration, success, and message, then dispatches to gRPC (in the background)
// and appends to the local rollback audit log.
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

	// Attach the session identity to every event so the backend can attribute
	// the rollback to a specific user session. We deliberately send ONLY the
	// session ID: the raw token never leaves the client in the event body. The
	// backend authenticates the request via the gRPC authorization metadata
	// attached by attachAuthMetadata, so putting the token here too would be a
	// second, unredacted copy of a credential in a serialized message.
	if sid, _ := loadSessionIdentity(); sid != "" {
		r.event.UserId = sid
	}

	// Enqueue for background gRPC send (non-blocking).
	gRPCQueued := enqueueRollbackEvent(r.event)

	// Append to local rollback audit log.
	r.writeLocalLog(step, success, msg, duration, errMsg, gRPCQueued)
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
func (r *RollbackReporter) writeLocalLog(step string, success bool, msg string, duration time.Duration, errMsg string, gRPCQueued bool) {
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

	grpcStatus := "grpc=dropped"
	if gRPCQueued {
		grpcStatus = "grpc=queued"
	}

	if msg != "" {
		line := fmt.Sprintf("%s [%s] %s -> %s step=%s dur=%s msg=%q %s %s\n",
			time.Now().Format(time.RFC3339),
			r.event.AppName,
			r.event.CurrentVersion,
			r.event.TargetVersion,
			step,
			duration.Round(time.Millisecond),
			msg,
			status,
			grpcStatus,
		)
		_, _ = f.WriteString(line)
	} else {
		line := fmt.Sprintf("%s [%s] %s -> %s step=%s dur=%s %s %s\n",
			time.Now().Format(time.RFC3339),
			r.event.AppName,
			r.event.CurrentVersion,
			r.event.TargetVersion,
			step,
			duration.Round(time.Millisecond),
			status,
			grpcStatus,
		)
		_, _ = f.WriteString(line)
	}
}

// ---------------------------------------------------------------------------
// Automatic version list sync
// ---------------------------------------------------------------------------

// SendVersionListForApp loads the version list for the given app and sends it
// to the backend as a rollback lifecycle event. This is called automatically
// after successful deploys and periodically by the monitor — the user never
// needs to run `phelix rollback --list` manually.
func SendVersionListForApp(appID, appName, appDir string) {
	vers, err := deploy.ListVersionsForDisplay(appName, deploy.DefaultRetention{Max: 5})
	if err != nil {
		grpcLog("[gRPC] Version sync: failed to list versions for %s: %v", appName, err)
		return
	}

	if len(vers) == 0 {
		return
	}

	// Build proto version entries.
	pbVersions := make([]*pb.RollbackVersionEntry, 0, len(vers))
	for _, v := range vers {
		pbVersions = append(pbVersions, &pb.RollbackVersionEntry{
			Version:        int32(v.Version),
			Tag:            v.Tag,
			GitCommit:      v.GitCommit,
			BuildTimestamp: v.BuiltAt.UnixMilli(),
			BinarySize:     v.SizeBytes,
			Current:        v.IsCurrent,
			PruneSoon:      deploy.WouldPruneOnNextBuild(appName, v.Version, deploy.DefaultRetention{Max: 5}),
		})
	}

	event := &pb.RollbackLifecycleEvent{
		CliAppId:      appID,
		ResolvedAppId: appID,
		AppName:       appName,
		CurrentStep:   RollbackStepListVersions,
		Success:       true,
		Message:       fmt.Sprintf("synced %d versions", len(vers)),
		Timestamp:     time.Now().UnixMilli(),
		Pid:           int32(os.Getpid()),
		Versions:      pbVersions,
		Metadata: map[string]string{
			"app_directory": appDir,
			"sync_mode":     "automatic",
			"version_count": fmt.Sprintf("%d", len(vers)),
		},
	}

	// Fill server ID and auth identity. The session token is intentionally not
	// carried in the event body — the request is authenticated via gRPC
	// authorization metadata, and the raw token must never be serialized into a
	// message that could be logged, forwarded, or inspected in transit.
	_ = server.Initialize()
	event.ServerId = server.GetServerID()
	if sid, _ := loadSessionIdentity(); sid != "" {
		event.UserId = sid
	}

	enqueueRollbackEvent(event)
	grpcLog("[gRPC] Version sync: queued %d versions for %s", len(vers), appName)
}
