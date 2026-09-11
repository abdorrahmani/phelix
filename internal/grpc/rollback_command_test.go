package grpc

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/monitor"
)

// Tests for remote rollback commands on the MonitorStream: payload mapping
// (including the ms → time.Duration boundary conversion), request_id
// preservation, structured error_code propagation, and duplicate request_id
// idempotency (a retried destructive rollback must not execute twice).

// resetRollbackResultCache clears the package-level dedup cache so tests
// never see results from a previous test (or a previous -count run).
func resetRollbackResultCache(t *testing.T) {
	t.Helper()
	rollbackResults = testRollbackLedger(t, rollbackLedgerCapacity)
}

// recordingExecutor captures every command and can fail on demand.
// Mutex-guarded: Execute runs on the stream goroutine while tests poll
// count() from the test goroutine.
type recordingExecutor struct {
	mu       sync.Mutex
	executed []monitor.Command
	fail     error
}

func (r *recordingExecutor) Execute(cmd monitor.Command) error {
	r.mu.Lock()
	r.executed = append(r.executed, cmd)
	r.mu.Unlock()
	return r.fail
}

func (r *recordingExecutor) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.executed)
}

// drainMonitorStream shuts the stream down and waits for the receive loop to
// return. Tests set the command settle delay to zero, so no post-command worker
// remains after the stream closes.
func drainMonitorStream(t *testing.T, c *Client, streamErrCh chan error) {
	t.Helper()
	close(c.done)
	select {
	case <-streamErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for monitor stream to shut down")
	}
}

// runRollbackCommand drives one MonitorStream session against the fake
// backend, pushes a rollback command, and returns the command result.
func runRollbackCommand(t *testing.T, exec *recordingExecutor, req *pb.MonitorCommandRequest, timeout time.Duration) *pb.MonitorCommandResult {
	t.Helper()
	setupTestSession(t)

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 10 * time.Second // no periodic ticks; only the post-command tick
	defer func() { monitorMetricsInterval = origInterval }()

	origCollector := monitorStream.metricsCollector
	resetRollbackResultCache(t)
	origExecutor := monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = exec
	defer func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	}()

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	go func() { streamErrCh <- c.runMonitorStream() }()

	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for initial ServerInfo event")
	}

	backend.toClient <- &pb.MonitorControl{
		Payload: &pb.MonitorControl_Command{Command: req},
	}

	result := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		res, ok := ev.Payload.(*pb.MonitorEvent_CommandResult)
		return ok && res.CommandResult.GetRequestId() == req.GetRequestId()
	}, timeout)
	if result == nil {
		t.Fatal("timed out waiting for rollback command result")
	}

	drainMonitorStream(t, c, streamErrCh)

	return result.Payload.(*pb.MonitorEvent_CommandResult).CommandResult
}

func TestMonitorStream_RollbackCommandFieldsReachExecutor(t *testing.T) {
	exec := &recordingExecutor{}
	result := runRollbackCommand(t, exec, &pb.MonitorCommandRequest{
		RequestId:        "req-rb-1",
		Type:             "rollback",
		AppName:          "shop",
		Target:           "v7",
		Reason:           "login 500",
		VerifyDurationMs: 30000,
		DryRun:           true,
	}, 3*time.Second)

	if exec.count() != 1 {
		t.Fatalf("executor called %d times, want 1", exec.count())
	}
	got := exec.executed[0].Payload
	if got.Target != "v7" || got.Reason != "login 500" || !got.DryRun {
		t.Fatalf("rollback options did not reach the executor: %+v", got)
	}
	// 30000 ms must arrive as 30s — converted once at the transport boundary.
	if got.VerifyDuration != 30*time.Second {
		t.Fatalf("verify duration = %s, want 30s", got.VerifyDuration)
	}
	if got.RequestID != "req-rb-1" {
		t.Fatalf("request_id = %q, want req-rb-1", got.RequestID)
	}

	if result.GetStatus() != "success" {
		t.Fatalf("status = %q (error=%q)", result.GetStatus(), result.GetError())
	}
	if result.GetRequestId() != "req-rb-1" || result.GetCommand() != "rollback" || result.GetAppName() != "shop" {
		t.Fatalf("result identity fields wrong: %+v", result)
	}
	if result.GetErrorCode() != "" {
		t.Fatalf("success result must carry no error_code, got %q", result.GetErrorCode())
	}
}

func TestMonitorStream_RollbackCommandErrorPropagates(t *testing.T) {
	structured := phelixerr.New(phelixerr.CodeRollbackTargetNotFound, "version v99 does not exist")
	exec := &recordingExecutor{fail: structured}
	result := runRollbackCommand(t, exec, &pb.MonitorCommandRequest{
		RequestId: "req-rb-err",
		Type:      "rollback",
		AppName:   "shop",
		Target:    "v99",
	}, 3*time.Second)

	if result.GetStatus() != "error" {
		t.Fatalf("status = %q, want error", result.GetStatus())
	}
	if result.GetErrorCode() != "ROLLBACK_TARGET_NOT_FOUND" {
		t.Fatalf("error_code = %q, want ROLLBACK_TARGET_NOT_FOUND", result.GetErrorCode())
	}
	if result.GetError() != structured.Error() {
		t.Fatalf("error text = %q, want %q", result.GetError(), structured.Error())
	}
}

// TestMonitorStream_RollbackDuplicateRequestNotReexecuted pins the remote
// rollback idempotency guarantee: the same request_id arriving twice (a
// backend retry after a lost result) executes once; the second delivery
// replays the recorded result.
func TestMonitorStream_RollbackDuplicateRequestNotReexecuted(t *testing.T) {
	setupTestSession(t)

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 10 * time.Second // no periodic ticks; only post-command ticks
	defer func() { monitorMetricsInterval = origInterval }()

	exec := &recordingExecutor{}
	origCollector := monitorStream.metricsCollector
	resetRollbackResultCache(t)
	origExecutor := monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = exec
	defer func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	}()

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	go func() { streamErrCh <- c.runMonitorStream() }()

	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for initial ServerInfo event")
	}

	cmd := &pb.MonitorCommandRequest{
		RequestId: "req-rb-dup",
		Type:      "rollback",
		AppName:   "shop",
		Target:    "v2",
	}
	backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: cmd}}
	backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: cmd}}

	// Both deliveries must produce a result with the same request_id.
	results := 0
	deadline := time.After(3 * time.Second)
	for results < 2 {
		ev := backend.eventsWithPayload(func(e *pb.MonitorEvent) bool {
			res, ok := e.Payload.(*pb.MonitorEvent_CommandResult)
			return ok && res.CommandResult.GetRequestId() == "req-rb-dup"
		}, 3*time.Second)
		if ev == nil {
			break
		}
		results++
		select {
		case <-deadline:
			t.Fatal("timed out collecting duplicate results")
		default:
		}
	}
	if results != 2 {
		t.Fatalf("collected %d results, want 2 (one per delivery)", results)
	}
	if exec.count() != 1 {
		t.Fatalf("rollback executed %d times for one request_id, want exactly 1", exec.count())
	}

	drainMonitorStream(t, c, streamErrCh)

	if exec.count() != 1 {
		t.Fatalf("rollback executed %d times for one request_id, want exactly 1", exec.count())
	}
}

// A rollback command without request_id is rejected deterministically: an
// uncorrelated destructive command can never be deduplicated or audited.
func TestMonitorStream_RollbackWithoutRequestIDRejected(t *testing.T) {
	exec := &recordingExecutor{}

	// Same harness, but the result has no request_id to wait on — collect
	// any rollback error result instead.
	setupTestSession(t)
	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 10 * time.Second // no periodic ticks; only post-command ticks
	defer func() { monitorMetricsInterval = origInterval }()
	origCollector := monitorStream.metricsCollector
	resetRollbackResultCache(t)
	origExecutor := monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = exec
	defer func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	}()

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	go func() { streamErrCh <- c.runMonitorStream() }()

	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for initial ServerInfo event")
	}

	backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{
		Command: &pb.MonitorCommandRequest{Type: "rollback", AppName: "shop", Target: "v2"},
	}}

	ev := backend.eventsWithPayload(func(e *pb.MonitorEvent) bool {
		_, ok := e.Payload.(*pb.MonitorEvent_CommandResult)
		return ok
	}, 3*time.Second)
	if ev == nil {
		t.Fatal("timed out waiting for rejection result")
	}
	res := ev.Payload.(*pb.MonitorEvent_CommandResult).CommandResult
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("rejection wrong: status=%q error_code=%q", res.GetStatus(), res.GetErrorCode())
	}
	if exec.count() != 0 {
		t.Fatalf("executor ran %d times for an uncorrelated rollback, want 0", exec.count())
	}

	drainMonitorStream(t, c, streamErrCh)
}

// Non-rollback commands keep their pre-existing behavior: no dedup, a repeat
// re-executes (restarts are idempotent-enough and cheap; rollback is the only
// destructive command on the channel).
func TestMonitorStream_NonRollbackCommandsNotDeduped(t *testing.T) {
	setupTestSession(t)

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 10 * time.Second // no periodic ticks; only post-command ticks
	defer func() { monitorMetricsInterval = origInterval }()

	exec := &recordingExecutor{}
	origCollector := monitorStream.metricsCollector
	resetRollbackResultCache(t)
	origExecutor := monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = exec
	defer func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	}()

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	go func() { streamErrCh <- c.runMonitorStream() }()

	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for initial ServerInfo event")
	}

	for i := 0; i < 2; i++ {
		backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{
			Command: &pb.MonitorCommandRequest{RequestId: "req-restart-dup", Type: "restart", AppName: "shop"},
		}}
	}

	deadline := time.Now().Add(10 * time.Second)
	for exec.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("restart executed %d times, want 2 (no dedup for lifecycle commands)", exec.count())
		}
		time.Sleep(10 * time.Millisecond)
	}

	drainMonitorStream(t, c, streamErrCh)
}

func TestMonitorStream_RollbackVerifyDurationOutOfRangeRejectedBeforeConversion(t *testing.T) {
	for i, raw := range []int64{-1, maxRemoteVerifyDurationMS + 1, int64(^uint64(0) >> 1)} {
		exec := &recordingExecutor{}
		result := runRollbackCommand(t, exec, &pb.MonitorCommandRequest{
			RequestId:        fmt.Sprintf("req-duration-%d", i),
			Type:             "rollback",
			AppName:          "shop",
			VerifyDurationMs: raw,
		}, 3*time.Second)
		if result.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
			t.Fatalf("raw %d error_code = %q", raw, result.GetErrorCode())
		}
		if exec.count() != 0 {
			t.Fatalf("raw %d reached executor", raw)
		}
	}
}

func TestMonitorStream_RollbackSameIDDifferentPayloadRejected(t *testing.T) {
	l := testRollbackLedger(t, 4)
	rollbackResults = l
	if _, _, err := l.begin(rollbackRequest("same-id", "v2")); err != nil {
		t.Fatal(err)
	}
	if err := l.complete("same-id", &pb.MonitorCommandResult{RequestId: "same-id", Command: "rollback", Status: "success"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := l.begin(rollbackRequest("same-id", "v3"))
	if !phelixerr.IsCode(err, phelixerr.CodeAlreadyExists) {
		t.Fatalf("error = %v, want ALREADY_EXISTS", err)
	}
}

// A plain executor error receives UNKNOWN so every failed terminal result has
// a machine-readable classification.
func TestMonitorStream_PlainExecutorErrorUsesUnknownCode(t *testing.T) {
	exec := &recordingExecutor{fail: errors.New("plain failure")}
	result := runRollbackCommand(t, exec, &pb.MonitorCommandRequest{
		RequestId: "req-plain",
		Type:      "rollback",
		AppName:   "shop",
		Target:    "v2",
	}, 3*time.Second)

	if result.GetStatus() != "error" || result.GetErrorCode() != string(phelixerr.CodeUnknown) {
		t.Fatalf("status=%q error_code=%q, want error with UNKNOWN", result.GetStatus(), result.GetErrorCode())
	}
}
