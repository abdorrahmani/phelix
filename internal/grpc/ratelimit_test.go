package grpc

// ratelimit_test.go verifies the CLI's handling of the backend's 2026-09-11
// abuse-resistance error shapes (docs/CLI_CHANGES_REQUIRED.md §2, items
// E1–E5 and C3) plus the agent-scoped session selection (§1) and the
// ServerInfo write-only field guarantee (§4 H3):
//
//   - ResourceExhausted on an auth attempt → minutes-scale backoff (E2);
//   - ResourceExhausted "message rate exceeded" on a stream → ≥30s before
//     the next attempt, not the 2s cadence (E1);
//   - ResourceExhausted "too many concurrent agent streams" → surfaced
//     clearly, no fast retry (E3);
//   - ResourceExhausted at send time (4 MiB) → logged with the event type,
//     never silent (E4);
//   - DeadlineExceeded "stream idle timeout" → normal reconnect cadence (E5);
//   - loadSession prefers the daemon's agent-scoped session file;
//   - toProtoServerInfo never serializes ssh_password/private_key.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// E2: failed-auth budget
// ---------------------------------------------------------------------------

func TestAuthBudgetErrorClassification(t *testing.T) {
	err := status.Error(codes.ResourceExhausted,
		"too many failed authentication attempts; retry after 5m0s")
	if !authBudgetError(err) {
		t.Fatal("failed-auth budget error not classified")
	}
	// Wrapped through the phelixerr chain, as callers receive it.
	wrapped := rpcFailed("attach auth metadata", err)
	if !authBudgetError(wrapped) {
		t.Fatal("wrapped failed-auth budget error not classified")
	}

	notBudget := status.Error(codes.ResourceExhausted, "message rate exceeded")
	if authBudgetError(notBudget) {
		t.Fatal("stream-rate error misclassified as auth budget")
	}
	if authBudgetError(status.Error(codes.Unavailable, "connection refused")) {
		t.Fatal("connection error misclassified as auth budget")
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		msg    string
		want   time.Duration
		wantOK bool
	}{
		{"too many failed authentication attempts; retry after 5m0s", 5 * time.Minute, true},
		{"too many failed authentication attempts; retry after 30s", 30 * time.Second, true},
		{"too many failed authentication attempts", 0, false},
		{"rate exceeded; retry after", 0, false},
		{"retry after not-a-duration", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.msg)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)",
				tc.msg, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestRateLimitedAuthErrorUsesServerWindow(t *testing.T) {
	err := rateLimitedAuthError(status.Error(codes.ResourceExhausted,
		"too many failed authentication attempts; retry after 7m30s"))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "7m30s") {
		t.Fatalf("error should carry the server-announced window: %q", msg)
	}

	// The announced window must also flow into the stream-loop policy:
	// 7m30s > the 5m default, so the delay must be 7m30s.
	policy := classifyStreamError(status.Error(codes.ResourceExhausted,
		"too many failed authentication attempts; retry after 7m30s"))
	if policy.delay != 450*time.Second {
		t.Fatalf("policy delay = %v, want the server-announced 7m30s", policy.delay)
	}

	// A window shorter than the default must not shorten the default.
	policy = classifyStreamError(status.Error(codes.ResourceExhausted,
		"too many failed authentication attempts; retry after 30s"))
	if policy.delay != authBudgetBackoff {
		t.Fatalf("policy delay = %v, want the %v floor, not the server's shorter window",
			policy.delay, authBudgetBackoff)
	}
}

// metadataBudgetBackend rejects SyncMetadata with the failed-auth budget
// error, emulating a backend that stopped processing auth attempts.
type metadataBudgetBackend struct {
	pb.UnimplementedPhelixServiceServer
}

func (b *metadataBudgetBackend) SyncMetadata(context.Context, *pb.CLIMetadata) (*pb.MetadataResponse, error) {
	return nil, status.Error(codes.ResourceExhausted,
		"too many failed authentication attempts; retry after 5m0s")
}

// TestMetadataSync_ParksOnAuthBudget proves that hitting the failed-auth
// budget parks the metadata sync for the budget window instead of
// re-attempting on the next tick.
func TestMetadataSync_ParksOnAuthBudget(t *testing.T) {
	setupTestSession(t)

	// Shorten the budget window for the test (the production value is 5m;
	// the setter restores it).
	setRateLimitBackoffsForTest(t, 80*time.Millisecond, 30*time.Second, 60*time.Second)

	backend := &metadataBudgetBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	// First sync hits the budget and parks.
	c.sendMetadataOnce()
	c.authParkMu.Lock()
	parkedUntil := c.authParkUntil
	c.authParkMu.Unlock()
	if parkedUntil.IsZero() {
		t.Fatal("metadata sync did not park after hitting the auth budget")
	}
	if until := time.Until(parkedUntil); until <= 0 {
		t.Fatalf("park window should still be in the future, got %v", until)
	}

	// While parked, the sync must not fire — verify by checking that a
	// second call returns without touching the RPC path. There is no direct
	// counter on the backend's unary handler (cheap to add): assert via the
	// park-until timestamp being unchanged (a fresh attempt would re-set it
	// past the first deadline).
	time.Sleep(20 * time.Millisecond)
	c.sendMetadataOnce()
	c.authParkMu.Lock()
	parkedUntil2 := c.authParkUntil
	c.authParkMu.Unlock()
	if !parkedUntil2.Equal(parkedUntil) {
		t.Fatalf("parked sync re-attempted: park deadline moved %v -> %v", parkedUntil, parkedUntil2)
	}
}

// ---------------------------------------------------------------------------
// E1/E3/E5: stream error classification and loop backoff
// ---------------------------------------------------------------------------

func TestClassifyStreamError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
		wantFast   bool // true = normal fast reconnect (no budget pause)
	}{
		{
			name:       "monitor stream rate budget",
			err:        status.Error(codes.ResourceExhausted, "monitor stream message rate exceeded"),
			wantReason: "backend stream message-rate budget exceeded",
		},
		{
			name:       "agent stream rate budget",
			err:        status.Error(codes.ResourceExhausted, "agent stream message rate exceeded"),
			wantReason: "backend stream message-rate budget exceeded",
		},
		{
			name:       "concurrent stream cap",
			err:        status.Error(codes.ResourceExhausted, "account has too many concurrent agent streams"),
			wantReason: "account concurrent-stream cap reached",
		},
		{
			name:       "auth budget on stream",
			err:        status.Error(codes.ResourceExhausted, "too many failed authentication attempts; retry after 5m0s"),
			wantReason: "backend auth-attempt budget exhausted",
		},
		{
			name:     "idle timeout behaves like disconnect",
			err:      status.Error(codes.DeadlineExceeded, "stream idle timeout"),
			wantFast: true,
		},
		{
			name:     "plain unavailable behaves like disconnect",
			err:      status.Error(codes.Unavailable, "connection reset by peer"),
			wantFast: true,
		},
	}
	for _, tc := range cases {
		got := classifyStreamError(tc.err)
		if tc.wantFast {
			if got.reason != "" || got.delay != 0 {
				t.Errorf("%s: expected normal reconnect policy, got {reason=%q delay=%v}",
					tc.name, got.reason, got.delay)
			}
		} else {
			if got.reason != tc.wantReason {
				t.Errorf("%s: reason = %q, want %q", tc.name, got.reason, tc.wantReason)
			}
			if got.delay < 30*time.Second && !tc.wantFast {
				// E1 contract: ≥30s for the stream budget; auth budget is
				// minutes; cap is a long pause. Checked precisely below per
				// kind; this is the global floor for the rate budget.
				if tc.name != "concurrent stream cap" || got.delay < 60*time.Second {
					if tc.name != "auth budget on stream" || got.delay < 5*time.Minute {
						t.Errorf("%s: backoff %v is below the contract floor", tc.name, got.delay)
					}
				}
			}
		}
	}
}

func TestStreamBackoffsMeetContract(t *testing.T) {
	if authBudgetBackoff < 5*time.Minute {
		t.Errorf("authBudgetBackoff = %v, want ≥5m (E2: minutes, not seconds)", authBudgetBackoff)
	}
	if streamRateBackoff < 30*time.Second {
		t.Errorf("streamRateBackoff = %v, want ≥30s (E1)", streamRateBackoff)
	}
	if streamCapBackoff < 30*time.Second {
		t.Errorf("streamCapBackoff = %v, want a long pause ≥30s (E3)", streamCapBackoff)
	}
}

// TestStreamRateEscalation pins the "repeatedly" half of E1: consecutive
// budget terminations lengthen the pause (30s → 60s → …) up to the 5m cap,
// so a budget-tripping stream stops re-sending its snapshot burst at the
// floor delay.
func TestStreamRateEscalation(t *testing.T) {
	cases := []struct {
		consecutive int
		want        time.Duration
	}{
		{0, 30 * time.Second}, // defensive floor
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 90 * time.Second},
		{10, 5 * time.Minute}, // capped
		{100, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := streamRateEscalation(tc.consecutive); got != tc.want {
			t.Errorf("streamRateEscalation(%d) = %v, want %v", tc.consecutive, got, tc.want)
		}
	}
}

// rateBudgetBackend terminates MonitorStream after receiving the first event
// with the given ResourceExhausted error, counting each session.
type rateBudgetBackend struct {
	pb.UnimplementedPhelixServiceServer

	mu       chan struct{} // closed once; guards attempts
	attempts int
	reject   error
}

func newRateBudgetBackend(reject error) *rateBudgetBackend {
	return &rateBudgetBackend{mu: make(chan struct{}), reject: reject}
}

func (b *rateBudgetBackend) MonitorStream(stream pb.PhelixService_MonitorStreamServer) error {
	b.attempts++
	// Drain in the background so the client's sends succeed.
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()
	// Hold the stream open briefly so the client attaches, then reject.
	time.Sleep(30 * time.Millisecond)
	return b.reject
}

// TestMonitorStreamLoop_BackoffsOnRateBudget proves the loop waits for the
// (test-shortened) budget window before re-opening the stream: within the
// window there is exactly one attempt, and the second one only comes after
// the window elapses.
func TestMonitorStreamLoop_BackoffsOnRateBudget(t *testing.T) {
	setupTestSession(t)
	swapMonitorStreamDeps(t, fakeMetricsCollector{}, &fakeCommandExecutor{})

	// Budget window of 250ms for the test; the production value (30s) is
	// asserted separately in TestStreamBackoffsMeetContract.
	setRateLimitBackoffsForTest(t, 5*time.Minute, 250*time.Millisecond, 60*time.Second)

	backend := newRateBudgetBackend(
		status.Error(codes.ResourceExhausted, "monitor stream message rate exceeded"))
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	loopDone := make(chan struct{})
	go func() {
		c.monitorStreamLoop()
		close(loopDone)
	}()

	// Within the window: exactly one attempt.
	time.Sleep(150 * time.Millisecond)
	if n := backend.attempts; n != 1 {
		t.Fatalf("stream re-opened inside the budget window (attempts=%d, want 1)", n)
	}

	// After the window: a second attempt arrives.
	resumed := waitForCondition(2*time.Second, 20*time.Millisecond, func() bool {
		return backend.attempts >= 2
	})
	if !resumed {
		t.Fatalf("stream did not resume after the budget window elapsed (attempts=%d)", backend.attempts)
	}

	close(c.done)
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor stream loop goroutine did not exit after close")
	}
}

// capBackend rejects MonitorStream outright with the concurrent-stream cap
// error, counting attempts.
type capBackend struct {
	pb.UnimplementedPhelixServiceServer
	attempts int
}

func (b *capBackend) MonitorStream(pb.PhelixService_MonitorStreamServer) error {
	b.attempts++
	return status.Error(codes.ResourceExhausted, "account has too many concurrent agent streams")
}

// TestMonitorStreamLoop_CapErrorSurfacesWithoutStorm proves the account
// stream cap does not produce a fast retry loop: one attempt, then a pause
// far longer than the normal 2s cadence (shortened to 300ms for the test).
func TestMonitorStreamLoop_CapErrorSurfacesWithoutStorm(t *testing.T) {
	setupTestSession(t)
	swapMonitorStreamDeps(t, fakeMetricsCollector{}, &fakeCommandExecutor{})

	setRateLimitBackoffsForTest(t, 5*time.Minute, 30*time.Second, 300*time.Millisecond)

	backend := &capBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	loopDone := make(chan struct{})
	go func() {
		c.monitorStreamLoop()
		close(loopDone)
	}()

	// The normal cadence would re-open within ~2s; within the cap window
	// there must be exactly one attempt.
	time.Sleep(150 * time.Millisecond)
	if n := backend.attempts; n != 1 {
		t.Fatalf("cap error retried at the fast cadence (attempts=%d, want 1)", n)
	}

	close(c.done)
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor stream loop goroutine did not exit after close")
	}
}

// idleTimeoutBackend closes the stream with the DeadlineExceeded idle
// timeout, counting sessions.
type idleTimeoutBackend struct {
	pb.UnimplementedPhelixServiceServer
	attempts int
}

func (b *idleTimeoutBackend) MonitorStream(stream pb.PhelixService_MonitorStreamServer) error {
	b.attempts++
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()
	time.Sleep(30 * time.Millisecond)
	return status.Error(codes.DeadlineExceeded, "stream idle timeout")
}

// TestMonitorStreamLoop_IdleTimeoutReconnectsNormally proves the idle-timeout
// shape (E5) is treated like a disconnect: the loop re-opens on the normal
// (short) cadence rather than a budget pause.
func TestMonitorStreamLoop_IdleTimeoutReconnectsNormally(t *testing.T) {
	setupTestSession(t)
	swapMonitorStreamDeps(t, fakeMetricsCollector{}, &fakeCommandExecutor{})

	// Make budget windows long so a (wrong) budget classification would be
	// obvious; the idle timeout must NOT wait them out.
	setRateLimitBackoffsForTest(t, 5*time.Minute, 30*time.Second, 60*time.Second)

	backend := &idleTimeoutBackend{}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	loopDone := make(chan struct{})
	go func() {
		c.monitorStreamLoop()
		close(loopDone)
	}()

	// Two attempts must happen within the normal 2s cadence (stream runs
	// ~30ms + monitorStreamRetryDelay 2s ≈ 2.1s per cycle).
	resumed := waitForCondition(6*time.Second, 50*time.Millisecond, func() bool {
		return backend.attempts >= 2
	})
	if !resumed {
		t.Fatalf("idle-timeout did not reconnect on the normal cadence (attempts=%d)", backend.attempts)
	}

	close(c.done)
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor stream loop goroutine did not exit after close")
	}
}

// ---------------------------------------------------------------------------
// E4: oversized message at send time
// ---------------------------------------------------------------------------

// oversizedSendStream's Send always fails with the ResourceExhausted
// send-limit shape. The stream client type is a generic alias, so the
// interface itself is embedded rather than an Unimplemented base.
type oversizedSendStream struct {
	pb.PhelixService_MonitorStreamClient
}

func (s *oversizedSendStream) Send(*pb.MonitorEvent) error {
	return status.Error(codes.ResourceExhausted,
		"grpc: received message larger than max (4194305 vs. 4194304)")
}

// TestMonitorSend_OversizedEventLogsLoudly verifies the E4 requirement: a
// send-time ResourceExhausted produces a clear diagnostic naming the event's
// payload type and the 4 MiB limit, and the error is surfaced (not swallowed,
// not a panic).
func TestMonitorSend_OversizedEventLogsLoudly(t *testing.T) {
	setupTestSession(t)

	var loggedTypes []string
	prevLog := oversizedEventLog
	oversizedEventLog = func(payloadType string) { loggedTypes = append(loggedTypes, payloadType) }
	t.Cleanup(func() { oversizedEventLog = prevLog })

	prev := monitorStream.stream
	monitorStream.stream = &oversizedSendStream{}
	t.Cleanup(func() { monitorStream.stream = prev })

	err := monitorStream.send(&pb.MonitorEvent{
		ServerId:  "srv-1",
		Timestamp: time.Now().UnixMilli(),
		Payload:   &pb.MonitorEvent_AppInfo{AppInfo: &pb.ApplicationInfo{Name: "huge"}},
	})
	if err == nil {
		t.Fatal("send should fail when the backend rejects the message")
	}
	if len(loggedTypes) != 1 || loggedTypes[0] != "app_info" {
		t.Fatalf("E4 diagnostic should name the offending payload type once, got %v", loggedTypes)
	}

	msg := oversizedEventMessage("app_info")
	if !strings.Contains(msg, "4 MiB") {
		t.Fatalf("E4 diagnostic should mention the 4 MiB limit: %q", msg)
	}
	if !strings.Contains(msg, "app_info") {
		t.Fatalf("E4 diagnostic should name the payload type: %q", msg)
	}
}

// TestMonitorSend_NormalErrorNotFlaggedAsOversized pins the classification:
// a connection-flavored send failure must not raise the E4 bug diagnostic.
func TestMonitorSend_NormalErrorNotFlaggedAsOversized(t *testing.T) {
	setupTestSession(t)

	var logged int
	prevLog := oversizedEventLog
	oversizedEventLog = func(string) { logged++ }
	t.Cleanup(func() { oversizedEventLog = prevLog })

	prev := monitorStream.stream
	monitorStream.stream = &failingSendStream{err: status.Error(codes.Unavailable, "connection reset")}
	t.Cleanup(func() { monitorStream.stream = prev })

	if err := monitorStream.send(&pb.MonitorEvent{}); err == nil {
		t.Fatal("send should fail")
	}
	if logged != 0 {
		t.Fatalf("connection failure wrongly flagged as oversized (logged=%d)", logged)
	}
}

// failingSendStream's Send fails with a caller-chosen error.
type failingSendStream struct {
	pb.PhelixService_MonitorStreamClient
	err error
}

func (s *failingSendStream) Send(*pb.MonitorEvent) error { return s.err }

func TestEventPayloadName(t *testing.T) {
	cases := []struct {
		event *pb.MonitorEvent
		want  string
	}{
		{&pb.MonitorEvent{Payload: &pb.MonitorEvent_ServerInfo{ServerInfo: &pb.ServerInfo{}}}, "server_info"},
		{&pb.MonitorEvent{Payload: &pb.MonitorEvent_LogEntry{LogEntry: &pb.MonitorLogEntry{}}}, "log_entry"},
		{&pb.MonitorEvent{Payload: &pb.MonitorEvent_MatrixRun{MatrixRun: &pb.MatrixRunState{}}}, "matrix_run"},
		{&pb.MonitorEvent{}, "unknown"},
		{nil, "<nil>"},
	}
	for _, tc := range cases {
		if got := eventPayloadName(tc.event); got != tc.want {
			t.Errorf("eventPayloadName(%v) = %q, want %q", tc.event, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// §1 (B1): the gRPC client prefers the daemon's agent-scoped session
// ---------------------------------------------------------------------------

func writeSessionFileWithScope(t *testing.T, name string, fields map[string]any) string {
	t.Helper()
	phelixDir := filepath.Join(os.Getenv("HOME"), ".phelix")
	if err := os.MkdirAll(phelixDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(phelixDir, name)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	return path
}

func TestLoadSession_PrefersAgentSession(t *testing.T) {
	setupTestSession(t) // writes a full-scope session.json + isolated HOME

	writeSessionFileWithScope(t, "agent-session.json", map[string]any{
		"sessionID": "agent-sess-1",
		"token":     "agent-token",
		"scope":     "agent",
		"expiresAt": time.Now().Add(time.Hour),
	})

	s, err := loadSession()
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if s.SessionID != "agent-sess-1" || s.Token != "agent-token" {
		t.Fatalf("loadSession did not prefer the agent session: %+v", s)
	}
	if s.Scope != SessionScopeAgent {
		t.Fatalf("agent session scope = %q, want %q", s.Scope, SessionScopeAgent)
	}
}

func TestLoadSession_FallsBackToInteractiveSession(t *testing.T) {
	setupTestSession(t) // session.json only

	s, err := loadSession()
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if s.SessionID != "test-session" {
		t.Fatalf("loadSession should fall back to the interactive session, got %+v", s)
	}
}

func TestLoadSession_ExpiredAgentSessionFallsBack(t *testing.T) {
	setupTestSession(t)

	writeSessionFileWithScope(t, "agent-session.json", map[string]any{
		"sessionID": "agent-sess-expired",
		"token":     "agent-token",
		"scope":     "agent",
		"expiresAt": time.Now().Add(-time.Hour),
	})

	s, err := loadSession()
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if s.SessionID != "test-session" {
		t.Fatalf("an expired agent session must fall back to the interactive one, got %+v", s)
	}
}

// TestAttachAuthMetadata_UsesAgentSessionToken proves the wire metadata
// carries the agent-scoped session when the daemon file exists.
func TestAttachAuthMetadata_UsesAgentSessionToken(t *testing.T) {
	setupTestSession(t)
	if err := server.Initialize(); err != nil {
		t.Fatalf("server.Initialize: %v", err)
	}

	writeSessionFileWithScope(t, "agent-session.json", map[string]any{
		"sessionID": "agent-sess-1",
		"token":     "agent-token",
		"scope":     "agent",
		"expiresAt": time.Now().Add(time.Hour),
	})

	ctx, err := attachAuthMetadata(context.Background(), server.GetServerID())
	if err != nil {
		t.Fatalf("attachAuthMetadata: %v", err)
	}
	md, ok := extractOutgoingMetadata(ctx)
	if !ok {
		t.Fatal("no outgoing metadata on context")
	}
	if got := md.Get("token"); len(got) != 1 || got[0] != "agent-token" {
		t.Fatalf("token metadata = %v, want the agent session's token", got)
	}
	if got := md.Get("sessionid"); len(got) != 1 || got[0] != "agent-sess-1" {
		t.Fatalf("sessionid metadata = %v, want the agent session's id", got)
	}
}

// extractOutgoingMetadata pulls the outgoing metadata map back off a context
// (test counterpart of attachAuthMetadata).
func extractOutgoingMetadata(ctx context.Context) (metadata.MD, bool) {
	return metadata.FromOutgoingContext(ctx)
}

// TestPreferredSessionScope backs the monitor daemon's least-privilege
// startup warning: agent scope when the daemon file exists, full/"" when it
// does not, "" with no valid session at all.
func TestPreferredSessionScope(t *testing.T) {
	setupTestSession(t)
	if got := PreferredSessionScope(); got != "" {
		t.Fatalf("interactive-only setup: PreferredSessionScope = %q, want \"\"", got)
	}

	writeSessionFileWithScope(t, "agent-session.json", map[string]any{
		"sessionID": "agent-sess-1",
		"token":     "agent-token",
		"scope":     SessionScopeAgent,
		"expiresAt": time.Now().Add(time.Hour),
	})
	if got := PreferredSessionScope(); got != SessionScopeAgent {
		t.Fatalf("with agent session: PreferredSessionScope = %q, want %q", got, SessionScopeAgent)
	}
}

// TestMonitorStream_FirstEventIsPrompt pins the E5 pre-bind requirement: the
// ServerInfo snapshot must go out immediately after the stream opens, well
// inside the backend's 2-minute pre-bind idle window (a stream that sends
// nothing within that window is reaped).
func TestMonitorStream_FirstEventIsPrompt(t *testing.T) {
	setupTestSession(t)
	swapMonitorStreamDeps(t, fakeMetricsCollector{}, &fakeCommandExecutor{})

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	openedAt := time.Now()
	go func() {
		streamErrCh <- c.runMonitorStream()
	}()

	// The ServerInfo event must arrive within a couple of seconds — orders
	// of magnitude inside the 2-minute pre-bind window.
	sawServerInfo := waitForCondition(2*time.Second, 50*time.Millisecond, func() bool {
		return backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
			_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
			return ok
		}, 20*time.Millisecond) != nil
	})
	if !sawServerInfo {
		t.Fatal("no ServerInfo event within 2s of stream open — the stream would be reaped in the 2-minute pre-bind window")
	}
	if elapsed := time.Since(openedAt); elapsed > 2*time.Second {
		t.Fatalf("ServerInfo arrived too late: %v", elapsed)
	}

	close(c.done)
	select {
	case <-streamErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for monitor stream to shut down")
	}
}

// ---------------------------------------------------------------------------
// §4 (H3): ServerInfo never carries ssh_password / private_key
// ---------------------------------------------------------------------------

// TestServerInfo_NeverCarriesWriteOnlyFields is the regression guard for H3:
// even if some future code path populates ServerConnection.SSHPassword /
// PrivateKey (the settings-manager scrub is the first line of defense), the
// wire converter must blank them. Feed a fully-populated, maliciously-seeded
// ServerConnection through toProtoServerInfo and assert the secrets never
// survive the conversion.
func TestServerInfo_NeverCarriesWriteOnlyFields(t *testing.T) {
	info := &server.Info{
		ID:       "srv-1",
		Hostname: "web-1",
		Connection: &server.ServerConnection{
			SSHPort:     22,
			SSHUser:     "root",
			AuthMethod:  "password",
			SSHPassword: "synthetic-password-material",
			PrivateKey:  "-----BEGIN OPENSSH PRIVATE KEY-----\nsynthetic-key-material\n-----END OPENSSH PRIVATE KEY-----",
			PublicKey:   "ssh-ed25519 AAAA synthetic-public",
		},
	}

	out := toProtoServerInfo(info)
	if out == nil || out.Connection == nil {
		t.Fatal("connection group unexpectedly absent from the wire message")
	}
	if out.Connection.SshPassword != "" {
		t.Fatalf("ssh_password leaked onto the wire: %q", out.Connection.SshPassword)
	}
	if out.Connection.PrivateKey != "" {
		t.Fatalf("private_key leaked onto the wire: %q", out.Connection.PrivateKey)
	}
	// The non-secret connection fields keep flowing.
	if out.Connection.SshPort != 22 || out.Connection.SshUser != "root" || out.Connection.PublicKey != "ssh-ed25519 AAAA synthetic-public" {
		t.Fatalf("non-secret connection fields disturbed: %+v", out.Connection)
	}
}

// TestServerConnection_ZeroValueOnWire pins the honest-agent invariant end
// to end: the settings manager (EnsureSettings) produces a zero Connection
// group, so the wire message carries an empty group even when seeded.
func TestServerConnection_ZeroValueOnWire(t *testing.T) {
	setupTestSession(t) // may seed settings.json with connection values

	s := server.EnsureSettings()
	out := toProtoServerConnection(&s.Connection)
	if out == nil {
		t.Fatal("nil connection on the wire")
	}
	if out.SshPassword != "" || out.PrivateKey != "" {
		t.Fatalf("settings manager let key material through: %+v", out)
	}
}
