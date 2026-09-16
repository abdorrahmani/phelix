package grpc

// channel_security_test.go verifies the security properties of the monitor
// channel:
//
//   - Application and self log content is redacted before it reaches the
//     wire (a secret printed by a managed app must never arrive at the
//     backend unfiltered).
//   - A backend auth rejection (gRPC Unauthenticated) stops the stream loops
//     instead of retrying forever with a revoked token.
//   - The stream loop parks when no local session exists, and resumes once
//     a session is written again.
//   - Error strings embedded in command results and application events are
//     redacted before serialization.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/connstate"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// secretLogsCollector returns log entries laden with realistic secret
// material, standing in for a managed application that prints its
// DATABASE_URL and API tokens to stdout/stderr.
type secretLogsCollector struct{ fakeMetricsCollector }

func (secretLogsCollector) CollectAppLogs() ([]logs.LogEntry, error) {
	return []logs.LogEntry{{
		ID:       "app-1",
		ServerID: "srv-1",
		AppID:    "app-1",
		Log:      "db connect failed: DATABASE_URL=postgres://admin:hunter2@db.internal:5432/prod token=ghp_abcdef1234567890",
		Date:     time.Now(),
		Level:    logs.LevelError,
		Stream:   logs.StreamStderr,
	}}, nil
}

func (secretLogsCollector) CollectSelfLogs() ([]logs.LogEntry, error) {
	return []logs.LogEntry{{
		ID:       "srv-1",
		ServerID: "srv-1",
		Log:      "subprocess failed: password=supersecret",
		Date:     time.Now(),
		Level:    logs.LevelWarning,
	}}, nil
}

// authRejectingBackend rejects every MonitorStream with gRPC Unauthenticated,
// emulating a backend that has revoked the session the agent presents.
type authRejectingBackend struct {
	pb.UnimplementedPhelixServiceServer

	mu       sync.Mutex
	attempts int
}

func (b *authRejectingBackend) MonitorStream(pb.PhelixService_MonitorStreamServer) error {
	b.mu.Lock()
	b.attempts++
	b.mu.Unlock()
	return status.Error(codes.Unauthenticated, "session revoked")
}

func (b *authRejectingBackend) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// countingBackend keeps MonitorStream sessions open and counts how many were
// opened, so a test can prove the client did (or did not) retry.
type countingBackend struct {
	pb.UnimplementedPhelixServiceServer

	mu       sync.Mutex
	attempts int
}

func (b *countingBackend) MonitorStream(stream pb.PhelixService_MonitorStreamServer) error {
	b.mu.Lock()
	b.attempts++
	b.mu.Unlock()
	// Drain until the client goes away; keep the session open meanwhile.
	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}
	}
}

func (b *countingBackend) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// writeTestSessionFile writes a valid, non-expired session under the current
// HOME, matching the on-disk format loadSession reads.
func writeTestSessionFile(t *testing.T) {
	t.Helper()
	phelixDir := filepath.Join(os.Getenv("HOME"), ".phelix")
	if err := os.MkdirAll(phelixDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"sessionID": "test-session",
		"token":     "test-token",
		"expiresAt": time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(phelixDir, "session.json"), data, 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
}

func removeTestSessionFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".phelix", "session.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove session: %v", err)
	}
}

// swapMonitorStreamDeps replaces the global monitor stream collector and
// executor for the duration of one test.
func swapMonitorStreamDeps(t *testing.T, collector monitor.MetricsCollector, executor *fakeCommandExecutor) {
	t.Helper()
	origCollector := monitorStream.metricsCollector
	origExecutor := monitorStream.commandExecutor
	monitorStream.metricsCollector = collector
	monitorStream.commandExecutor = executor
	t.Cleanup(func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	})
}

// ---------------------------------------------------------------------------
// Streamed-log redaction
// ---------------------------------------------------------------------------

// TestMonitorStream_RedactsStreamedLogs is the end-to-end proof that a secret
// printed by a managed application never reaches the backend unfiltered: the
// log entry traverses the collector → converter → MonitorStream wire path and
// arrives with the credential material masked.
func TestMonitorStream_RedactsStreamedLogs(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 20 * time.Millisecond
	defer func() { monitorMetricsInterval = origInterval }()

	swapMonitorStreamDeps(t, secretLogsCollector{}, &fakeCommandExecutor{})

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	go func() {
		streamErrCh <- c.runMonitorStream()
	}()

	// App log: the DATABASE_URL password and the GitHub token must be masked,
	// while the rest of the line stays useful for debugging.
	appLog := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		entry, ok := ev.Payload.(*pb.MonitorEvent_LogEntry)
		return ok && entry.LogEntry.GetSource() == pb.LogSource_LOG_SOURCE_APPLICATION
	}, 2*time.Second)
	if appLog == nil {
		t.Fatal("timed out waiting for an app log event")
	}
	appLine := appLog.Payload.(*pb.MonitorEvent_LogEntry).LogEntry.GetLog()
	if strings.Contains(appLine, "hunter2") || strings.Contains(appLine, "ghp_abcdef1234567890") {
		t.Fatalf("app log leaked secret material over the wire: %q", appLine)
	}
	if !strings.Contains(appLine, "postgres://admin:***@db.internal:5432/prod") {
		t.Fatalf("app log should keep scheme/user/host with the password masked, got: %q", appLine)
	}

	// Self log: the daemon's own log line is redacted with the same rules.
	selfLog := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		entry, ok := ev.Payload.(*pb.MonitorEvent_LogEntry)
		return ok && entry.LogEntry.GetSource() == pb.LogSource_LOG_SOURCE_SELF
	}, 2*time.Second)
	if selfLog == nil {
		t.Fatal("timed out waiting for a self log event")
	}
	selfLine := selfLog.Payload.(*pb.MonitorEvent_LogEntry).LogEntry.GetLog()
	if strings.Contains(selfLine, "supersecret") {
		t.Fatalf("self log leaked secret material over the wire: %q", selfLine)
	}
	if !strings.Contains(selfLine, "***") {
		t.Fatalf("self log should carry a redaction marker, got: %q", selfLine)
	}

	close(c.done)
	select {
	case <-streamErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for monitor stream to shut down")
	}
}

// ---------------------------------------------------------------------------
// Auth rejection stops the loops
// ---------------------------------------------------------------------------

// TestMonitorStreamLoop_StopsOnAuthRejection proves that a revoked session
// does not produce an infinite 2-second retry loop: the backend answers
// Unauthenticated exactly once before the client tears itself down.
func TestMonitorStreamLoop_StopsOnAuthRejection(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)

	prevState := connstate.Get()
	t.Cleanup(func() { connstate.Set(prevState) })

	backend := &authRejectingBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	loopDone := make(chan struct{})
	go func() {
		c.monitorStreamLoop()
		close(loopDone)
	}()

	// handleAuthRejected must close the client (its done channel) and record
	// the auth_expired state.
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("auth rejection did not stop the monitor stream loop (client never closed)")
	}
	if got := connstate.Get(); got != connstate.AuthExpired {
		t.Fatalf("expected connection state %q after auth rejection, got %q", connstate.AuthExpired, got)
	}

	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor stream loop goroutine did not exit after auth rejection")
	}

	// The loop is gone, so no further attempts can happen; give a would-be
	// retry ample time to (wrongly) fire and prove it never does.
	time.Sleep(250 * time.Millisecond)
	if n := backend.attemptCount(); n != 1 {
		t.Fatalf("expected exactly 1 stream attempt against the rejecting backend, got %d", n)
	}
}

// TestAgentStreamLoop_StopsOnAuthRejection is the agent stream counterpart.
func TestAgentStreamLoop_StopsOnAuthRejection(t *testing.T) {
	setupTestSession(t)

	prevState := connstate.Get()
	t.Cleanup(func() { connstate.Set(prevState) })

	backend := &agentAuthRejectingBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	loopDone := make(chan struct{})
	go func() {
		c.startAgentStream()
		close(loopDone)
	}()

	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("auth rejection did not stop the agent stream loop (client never closed)")
	}
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("agent stream loop goroutine did not exit after auth rejection")
	}
	time.Sleep(250 * time.Millisecond)
	if n := backend.attemptCount(); n != 1 {
		t.Fatalf("expected exactly 1 agent stream attempt against the rejecting backend, got %d", n)
	}
}

// agentAuthRejectingBackend rejects AgentStream with Unauthenticated.
type agentAuthRejectingBackend struct {
	pb.UnimplementedPhelixServiceServer

	mu       sync.Mutex
	attempts int
}

func (b *agentAuthRejectingBackend) AgentStream(pb.PhelixService_AgentStreamServer) error {
	b.mu.Lock()
	b.attempts++
	b.mu.Unlock()
	return status.Error(codes.Unauthenticated, "session revoked")
}

func (b *agentAuthRejectingBackend) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// TestMonitorStreamLoop_ParksWithoutSession proves the loop neither dials nor
// spins while logged out, and that it picks the session back up (and opens a
// stream) as soon as 'phelix auth login' writes one.
func TestMonitorStreamLoop_ParksWithoutSession(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)
	removeTestSessionFile(t)

	origDelay := monitorStreamNoSessionDelay
	monitorStreamNoSessionDelay = 50 * time.Millisecond
	t.Cleanup(func() { monitorStreamNoSessionDelay = origDelay })

	backend := &countingBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	loopDone := make(chan struct{})
	go func() {
		c.monitorStreamLoop()
		close(loopDone)
	}()

	// Parked: several park-intervals pass with zero stream attempts.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := backend.attemptCount(); n != 0 {
			t.Fatalf("stream attempted without a session (attempts=%d)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Login: the session file reappears and the loop resumes within one
	// park-interval.
	writeTestSessionFile(t)
	resumed := waitForCondition(2*time.Second, 20*time.Millisecond, func() bool {
		return backend.attemptCount() >= 1
	})
	if !resumed {
		t.Fatal("stream loop did not resume after a session was written")
	}

	close(c.done)
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor stream loop goroutine did not exit after close")
	}
}

func waitForCondition(timeout, poll time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(poll)
	}
	return cond()
}

// ---------------------------------------------------------------------------
// Error-message redaction on the wire
// ---------------------------------------------------------------------------

func TestCommandErrorResult_Redacted(t *testing.T) {
	req := &pb.MonitorCommandRequest{RequestId: "r-1", Type: "restart", AppName: "shop"}
	res := commandErrorResult(req, errors.New("stop failed: password=hunter2 token=abc123xyz"))
	if strings.Contains(res.GetError(), "hunter2") || strings.Contains(res.GetError(), "abc123xyz") {
		t.Fatalf("command error result leaked credentials: %q", res.GetError())
	}
	if !strings.Contains(res.GetError(), "***") {
		t.Fatalf("command error result should carry redaction markers: %q", res.GetError())
	}
	if res.GetStatus() != "error" || res.GetRequestId() != "r-1" {
		t.Fatalf("redaction must not disturb result metadata: %+v", res)
	}
}

func TestNewApplicationEvent_RedactsErrorMessage(t *testing.T) {
	ev := NewApplicationEvent("app-1", "shop", "stop", false,
		"signal failed: postgres://admin:hunter2@db.internal:5432/prod", 42, "classic", "v1.2.3")
	if strings.Contains(ev.GetErrorMessage(), "hunter2") {
		t.Fatalf("application event leaked the database password: %q", ev.GetErrorMessage())
	}
	if !strings.Contains(ev.GetErrorMessage(), "***") {
		t.Fatalf("application event error should be redacted: %q", ev.GetErrorMessage())
	}
}

// ---------------------------------------------------------------------------
// isAuthRejection
// ---------------------------------------------------------------------------

func TestIsAuthRejection(t *testing.T) {
	revoked := status.Error(codes.Unauthenticated, "session revoked")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"raw unauthenticated status", revoked, true},
		{"wrapped unauthenticated status", phelixerr.Wrap(phelixerr.CodeConnection, "open monitor stream", revoked), true},
		{"unavailable status", status.Error(codes.Unavailable, "backend down"), false},
		{"plain error", errors.New("connection refused"), false},
		// A LOCAL "no session" failure is not a backend rejection: it must not
		// trigger the auth-rejected shutdown path (the stream loop parks
		// instead, waiting for 'phelix auth login').
		{"local unauthenticated structured error", phelixerr.New(phelixerr.CodeUnauthenticated, "no active session found"), false},
	}
	for _, tc := range cases {
		if got := isAuthRejection(tc.err); got != tc.want {
			t.Errorf("%s: isAuthRejection = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIsAuthRejection_StreamRecvShape covers the exact error shape the recv
// loop produces: a gRPC status error returned by stream.Recv().
func TestIsAuthRejection_StreamRecvShape(t *testing.T) {
	err := status.Error(codes.Unauthenticated, "auth revoked")
	if !isAuthRejection(err) {
		t.Fatal("recv-loop auth error must be detected")
	}
}

var _ = context.Background // keep context import if future doubles need it
