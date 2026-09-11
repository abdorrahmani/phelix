package grpc

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/protobuf/proto"
)

// RollbackReporter emits lifecycle events for every significant step of a
// rollback. The backend receives each event independently, enabling real-time
// progress display, audit trail generation, and failure diagnosis without
// accessing CLI logs.
type RollbackReporter struct {
	event *pb.RollbackLifecycleEvent
	// localLog, when false, suppresses the local rollback.log append. Dry-run
	// previews use it: a preview must not mutate audit state, even locally.
	localLog bool
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

	// RollbackStepPreview Dry-run preview (read-only; no execution follows
	// necessarily — the operator may cancel).
	RollbackStepPreview = "preview"

	// RollbackStepHistory History sync (read-only; carries the structured
	// history records in the history field).
	RollbackStepHistory = "history"

	// RollbackStepVerifyPassed / RollbackStepVerifyFailed are the terminal
	// verification events. A cancelled window reports step=verify_failed with
	// verify_status="cancelled" — the outcome lives in the status field, the
	// step only distinguishes pass from not-pass.
	RollbackStepVerifyPassed = "verify_passed"
	RollbackStepVerifyFailed = "verify_failed"

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
		logs.InfoFile("grpc", "[gRPC] Rollback sender: background worker started")
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
			logs.InfoFile("grpc", "[gRPC] Rollback sender: closing stale connection")
			cachedClient.Close()
			cachedClient = nil
		}

		c := NewClient()
		if err := c.Connect(); err != nil {
			logs.ErrorFile("grpc", "[gRPC] Rollback sender: failed to connect: %v", err)
			return nil
		}
		cachedClient = c
		logs.InfoFile("grpc", "[gRPC] Rollback sender: connected to backend")
		return cachedClient
	}

	// cleanup closes the cached connection when the channel is drained.
	cleanup := func() {
		cachedMu.Lock()
		if cachedClient != nil {
			logs.InfoFile("grpc", "[gRPC] Rollback sender: closing connection after draining")
			cachedClient.Close()
			cachedClient = nil
		}
		cachedMu.Unlock()
	}

	logs.InfoFile("grpc", "[gRPC] Rollback sender: waiting for events...")

	for event := range rollbackEventCh {
		sanitizeEventStrings(event)

		step := event.GetCurrentStep()
		app := event.GetAppName()
		logs.InfoFile("grpc", "[gRPC] Rollback sender: processing event step=%s app=%s", step, app)

		c := ensureClient()
		if c == nil {
			failCount++
			logs.WarningFile("grpc", "[gRPC] Rollback sender: NO CONNECTION — dropping event step=%s app=%s (failures=%d)", step, app, failCount)
			continue
		}

		// Try full event first.
		if c.SendRollbackEvent(event) {
			sentCount++
			logs.InfoFile("grpc", "[gRPC] Rollback sender: sent step=%s app=%s (sent=%d)", step, app, sentCount)
			continue
		}

		failCount++
		logs.ErrorFile("grpc", "[gRPC] Rollback sender: SEND FAILED step=%s app=%s (failures=%d)", step, app, failCount)
		cachedMu.Lock()
		if cachedClient != nil {
			cachedClient.Close()
			cachedClient = nil
		}
		cachedMu.Unlock()
	}

	cleanup()
	logs.InfoFile("grpc", "[gRPC] Rollback sender: stopped (sent=%d, failed=%d)", sentCount, failCount)
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
		logs.InfoFile("grpc", "[gRPC] Rollback sender: no sender to stop")
		return
	}

	// Close the channel so the loop exits after draining.
	close(ch)
	rollbackEventCh = nil
	rollbackSenderDone = nil

	// Reset the Once so a future rollback can start a new sender.
	rollbackEventOnce = sync.Once{}

	logs.InfoFile("grpc", "[gRPC] Rollback sender: flushing (timeout=%s)...", timeout)

	select {
	case <-done:
		logs.InfoFile("grpc", "[gRPC] Rollback sender: flush complete")
	case <-time.After(timeout):
		logs.WarningFile("grpc", "[gRPC] Rollback sender: flush timed out after %s", timeout)
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
		logs.InfoFile("grpc", "[gRPC] Rollback event queued: step=%s app=%s", event.GetCurrentStep(), event.GetAppName())
		return true
	default:
		logs.WarningFile("grpc", "[gRPC] Rollback event channel full, DROPPED step=%s app=%s", event.GetCurrentStep(), event.GetAppName())
		return false
	}
}

// cloneRollbackEvent performs a deep copy of the proto so the background
// sender can safely read it after the caller mutates the original.
// proto.Clone is the correct way to copy a generated message: struct
// assignment would shallow-copy the embedded protoimpl.MessageState (which
// contains a noCopy mutex), and the map/slice fields would alias the source.
func cloneRollbackEvent(e *pb.RollbackLifecycleEvent) *pb.RollbackLifecycleEvent {
	return proto.Clone(e).(*pb.RollbackLifecycleEvent)
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
	e.ErrorCode = sanitizeUTF8(e.ErrorCode)
	e.Reason = sanitizeUTF8(e.Reason)
	e.VerifyStatus = sanitizeUTF8(e.VerifyStatus)
	e.VerifyError = sanitizeUTF8(e.VerifyError)
	e.TargetSource = sanitizeUTF8(e.TargetSource)
	e.Source = sanitizeUTF8(e.Source)
	e.RequestId = sanitizeUTF8(e.RequestId)

	if e.Preview != nil {
		p := e.Preview
		p.AppName = sanitizeUTF8(p.AppName)
		p.CurrentTag = sanitizeUTF8(p.CurrentTag)
		p.CurrentCommit = sanitizeUTF8(p.CurrentCommit)
		p.TargetTag = sanitizeUTF8(p.TargetTag)
		p.TargetCommit = sanitizeUTF8(p.TargetCommit)
		p.Strategy = sanitizeUTF8(p.Strategy)
		p.CurrentSlot = sanitizeUTF8(p.CurrentSlot)
		p.TargetSlot = sanitizeUTF8(p.TargetSlot)
		p.HealthCheck = sanitizeUTF8(p.HealthCheck)
		p.EnvSummary = sanitizeUTF8(p.EnvSummary)
		for i, s := range p.Steps {
			p.Steps[i] = sanitizeUTF8(s)
		}
		for i, s := range p.Warnings {
			p.Warnings[i] = sanitizeUTF8(s)
		}
	}

	for _, h := range e.History {
		h.FromVersion = sanitizeUTF8(h.FromVersion)
		h.ToVersion = sanitizeUTF8(h.ToVersion)
		h.Status = sanitizeUTF8(h.Status)
		h.Mode = sanitizeUTF8(h.Mode)
		h.Reason = sanitizeUTF8(h.Reason)
		h.Source = sanitizeUTF8(h.Source)
		h.Error = sanitizeUTF8(h.Error)
		h.VerifyStatus = sanitizeUTF8(h.VerifyStatus)
		h.VerifyDuration = sanitizeUTF8(h.VerifyDuration)
		h.VerifyError = sanitizeUTF8(h.VerifyError)
	}

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
		localLog: true,
	}
}

// DisableLocalLog suppresses the local rollback.log append for this reporter.
// Used by the dry-run preview: the preview is read-only and must not mutate
// audit state.
func (r *RollbackReporter) DisableLocalLog() {
	r.localLog = false
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
	if success || (step != RollbackStepFailed && step != RollbackStepVerifyFailed) {
		r.event.ErrorCode = ""
	}

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
	if r.localLog {
		r.writeLocalLog(step, success, msg, duration, errMsg, gRPCQueued)
	}
}

// SetVersionList populates the version list for --list events.
func (r *RollbackReporter) SetVersionList(versions []*pb.RollbackVersionEntry) {
	r.event.Versions = versions
}

// SetMetadata adds a key-value pair to the event metadata.
func (r *RollbackReporter) SetMetadata(key, value string) {
	r.event.Metadata[key] = value
	if key == "request_id" {
		r.event.RequestId = value
	}
}

// SetRequestID sets the typed command correlation field and preserves the
// legacy metadata fallback for backends that have not adopted field 32 yet.
func (r *RollbackReporter) SetRequestID(requestID string) {
	r.event.RequestId = requestID
	if requestID != "" {
		r.event.Metadata["request_id"] = requestID
	} else {
		delete(r.event.Metadata, "request_id")
	}
}

// SetErrorCode sets the machine-readable phelix error code for a failed
// terminal event (e.g. ROLLBACK_FAILED, ROLLBACK_VERIFY_FAILED). Must be a
// code from internal/errors/codes.go, never a free-form message.
func (r *RollbackReporter) SetErrorCode(code string) {
	r.event.ErrorCode = code
}

// EmitError emits a terminal failure and derives its machine-readable code
// from the typed error in the same operation. This prevents a terminal event
// from being queued before its error_code is assigned.
func (r *RollbackReporter) EmitError(step, msg string, duration time.Duration, err error) {
	if err == nil {
		err = phelixerr.New(phelixerr.CodeUnknown, "rollback failed")
	}
	r.event.ErrorCode = string(phelixerr.CodeOf(err))
	r.Emit(step, false, msg, duration, err.Error())
}

// SetReason attaches the validated rollback reason to every subsequent event
// of this rollback.
func (r *RollbackReporter) SetReason(reason string) {
	r.event.Reason = reason
}

// SetTargetSource records how the target was chosen ("explicit",
// "interactive" or "previous").
func (r *RollbackReporter) SetTargetSource(source string) {
	r.event.TargetSource = source
}

// SetVerification marks the rollback as carrying a verification request. The
// duration is recorded in milliseconds; the status/error fields are filled by
// EmitVerifyOutcome at the terminal verification event.
func (r *RollbackReporter) SetVerification(duration time.Duration) {
	r.event.VerifyRequested = true
	r.event.VerifyDurationMs = duration.Milliseconds()
}

// EmitVerifyOutcome reports the outcome of a requested verification window.
// It emits one terminal event so the backend timeline ends in a state that
// reflects whether the rollback truly held: a failed or cancelled window is
// reported via error_code=ROLLBACK_VERIFY_FAILED on this event — never via
// success=false on the earlier "complete" event, because the rollback
// execution itself committed.
func (r *RollbackReporter) EmitVerifyOutcome(status string, verifyErr string) {
	r.event.VerifyStatus = status
	r.event.VerifyError = verifyErr
	switch status {
	case deploy.RollbackVerifyFailed, deploy.RollbackVerifyCancelled:
		r.SetErrorCode(string(phelixerr.CodeRollbackVerifyFailed))
		r.Emit(RollbackStepVerifyFailed, false, "rollback verification "+status, 0, verifyErr)
	default: // passed
		r.SetErrorCode("")
		r.Emit(RollbackStepVerifyPassed, true, "rollback verification passed", 0, "")
	}
}

// SetPreview attaches the structured dry-run preview to the event.
func (r *RollbackReporter) SetPreview(p *pb.RollbackPreview) {
	r.event.Preview = p
}

// SetDryRun marks the event stream as a read-only dry-run report.
func (r *RollbackReporter) SetDryRun(dryRun bool) {
	r.event.DryRun = dryRun
}

// SetSource records the rollback origin ("manual" / "automatic"). Automatic
// recoveries are otherwise indistinguishable from operator rollbacks on the
// wire — the backend needs both to render an honest audit trail.
func (r *RollbackReporter) SetSource(source string) {
	r.event.Source = source
}

// SetHistory attaches structured history records to the event.
func (r *RollbackReporter) SetHistory(entries []*pb.RollbackHistoryEntry) {
	r.event.History = entries
}

// Event returns the underlying proto event for inspection or testing.
func (r *RollbackReporter) Event() *pb.RollbackLifecycleEvent {
	return r.event
}

// loadSessionIdentity reads the session ID and token from the preferred
// session file (agent-scoped when present, interactive otherwise) without
// failing the rollback if the session is missing or expired. Reading through
// loadSession keeps the attributed session ID consistent with the one whose
// credentials actually authenticate the RPC (attachAuthMetadata).
func loadSessionIdentity() (sessionID, token string) {
	s, err := loadSession()
	if err != nil {
		return "", ""
	}
	return s.SessionID, s.Token
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
	// Not logged in → skip the sync; the version history is still on disk and
	// will be uploaded once the user authenticates.
	if !sessionAvailable() {
		return
	}

	vers, err := deploy.ListVersionsForDisplay(appName, deploy.DefaultRetention{Max: 5})
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Version sync: failed to list versions for %s: %v", appName, err)
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
	logs.InfoFile("grpc", "[gRPC] Version sync: queued %d versions for %s", len(vers), appName)
}
