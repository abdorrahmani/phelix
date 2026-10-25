package grpc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// --- Validation -------------------------------------------------------------

func matrixReq(requestID, cmdType string, mutators ...func(*pb.MonitorCommandRequest)) *pb.MonitorCommandRequest {
	req := &pb.MonitorCommandRequest{RequestId: requestID, Type: cmdType, AppName: "myapp"}
	for _, m := range mutators {
		m(req)
	}
	return req
}

func TestValidateMatrixCommand(t *testing.T) {
	tests := []struct {
		name    string
		req     *pb.MonitorCommandRequest
		wantErr bool
	}{
		{name: "build minimal", req: matrixReq("r1", MatrixCommandBuild)},
		{name: "build with options", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
			r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}, Platforms: []string{"linux/amd64"}, Concurrency: 2, Retries: 1, Tag: "v1"}
		})},
		{name: "dockerize with docker options", req: matrixReq("r1", MatrixCommandDockerize, func(r *pb.MonitorCommandRequest) {
			r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}, Platforms: []string{"linux/amd64"}, Registry: "reg", Push: true}
		})},
		{name: "resume latest", req: matrixReq("r1", MatrixCommandResume, func(r *pb.MonitorCommandRequest) { r.Target = "latest" })},
		{name: "resume explicit id", req: matrixReq("r1", MatrixCommandResume, func(r *pb.MonitorCommandRequest) { r.Target = "mx_20260910_8f31" })},
		{name: "resume dry run", req: matrixReq("r1", MatrixCommandResume, func(r *pb.MonitorCommandRequest) { r.Target = "latest"; r.DryRun = true })},
		{name: "retry explicit id", req: matrixReq("r1", MatrixCommandRetry, func(r *pb.MonitorCommandRequest) { r.Target = "mx_20260910_8f31" })},
		{name: "status active", req: matrixReq("r1", MatrixCommandStatus)},
		{name: "status latest", req: matrixReq("r1", MatrixCommandStatus, func(r *pb.MonitorCommandRequest) { r.Target = "latest" })},
		{name: "status explicit id", req: matrixReq("r1", MatrixCommandStatus, func(r *pb.MonitorCommandRequest) { r.Target = "mx_20260910_8f31" })},
		{name: "list", req: matrixReq("r1", MatrixCommandList, func(r *pb.MonitorCommandRequest) {
			r.Matrix = &pb.MatrixOptions{ListLimit: 5}
		})},
		{name: "build dry run", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) { r.DryRun = true })},

		{name: "missing request id", req: matrixReq("", MatrixCommandBuild), wantErr: true},
		{name: "unknown type", req: matrixReq("r1", "matrix_nonsense"), wantErr: true},
		{name: "build missing app", req: &pb.MonitorCommandRequest{RequestId: "r1", Type: MatrixCommandBuild}, wantErr: true},
		{name: "build with target", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) { r.Target = "latest" }), wantErr: true},
		{name: "resume without target", req: matrixReq("r1", MatrixCommandResume), wantErr: true},
		{name: "retry without target", req: matrixReq("r1", MatrixCommandRetry), wantErr: true},
		{name: "retry with latest", req: matrixReq("r1", MatrixCommandRetry, func(r *pb.MonitorCommandRequest) { r.Target = "latest" }), wantErr: true},
		{name: "list with target", req: matrixReq("r1", MatrixCommandList, func(r *pb.MonitorCommandRequest) { r.Target = "mx_20260910_8f31" }), wantErr: true},
		{name: "foreign strategy", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) { r.Strategy = "rolling" }), wantErr: true},
		{name: "foreign replicas", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) { r.Replicas = 2 }), wantErr: true},
		{name: "foreign reason", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) { r.Reason = "why" }), wantErr: true},
		{name: "foreign verify duration", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) { r.VerifyDurationMs = 1000 }), wantErr: true},
		{name: "foreign canary percent", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) { r.CanaryPercent = 25 }), wantErr: true},
		{name: "dry run on retry", req: matrixReq("r1", MatrixCommandRetry, func(r *pb.MonitorCommandRequest) { r.Target = "mx_20260910_8f31"; r.DryRun = true }), wantErr: true},
		{name: "dry run on status", req: matrixReq("r1", MatrixCommandStatus, func(r *pb.MonitorCommandRequest) { r.DryRun = true }), wantErr: true},
		{name: "dry run on list", req: matrixReq("r1", MatrixCommandList, func(r *pb.MonitorCommandRequest) { r.DryRun = true }), wantErr: true},
		{name: "negative concurrency", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
			r.Matrix = &pb.MatrixOptions{Concurrency: -1}
		}), wantErr: true},
		{name: "negative retries", req: matrixReq("r1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
			r.Matrix = &pb.MatrixOptions{Retries: -3}
		}), wantErr: true},
		{name: "negative list limit", req: matrixReq("r1", MatrixCommandList, func(r *pb.MonitorCommandRequest) {
			r.Matrix = &pb.MatrixOptions{ListLimit: -1}
		}), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMatrixCommand(tc.req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateMatrixCommand() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// --- Ledger idempotency ------------------------------------------------------

// newTestLedger builds an initialized matrix ledger backed by a temp file.
func newTestLedger(t *testing.T) *rollbackLedger {
	t.Helper()
	l := newRollbackLedger(filepath.Join(t.TempDir(), "remote-matrix-ledger.json"), rollbackLedgerCapacity, "matrix", "test note")
	if err := l.initialize(); err != nil {
		t.Fatal(err)
	}
	return l
}

func successMatrixResult(req *pb.MonitorCommandRequest, runID string) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{
		RequestId:   req.GetRequestId(),
		Command:     req.GetType(),
		AppName:     req.GetAppName(),
		Status:      "success",
		Timestamp:   time.Now().UnixMilli(),
		MatrixRunId: runID,
	}
}

func TestMatrixLedger_BeginCompleteReplay(t *testing.T) {
	l := newTestLedger(t)
	req := matrixReq("req-1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
		r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}, Platforms: []string{"linux/amd64"}}
	})

	outcome, replay, err := l.begin(req)
	if err != nil || outcome != rollbackBeginExecute || replay != nil {
		t.Fatalf("begin: outcome=%v replay=%v err=%v", outcome, replay, err)
	}

	// A duplicate while in progress answers UNAVAILABLE — never re-executes.
	if _, _, err := l.begin(req); err == nil {
		t.Fatal("duplicate in-progress begin must fail")
	}

	stored := successMatrixResult(req, "mx_20260910_8f31")
	if err := l.complete(req.GetRequestId(), stored); err != nil {
		t.Fatal(err)
	}
	if err := l.markDelivered(req.GetRequestId()); err != nil {
		t.Fatal(err)
	}

	// Same request_id + same payload → replay of the stored result.
	outcome, replay, err = l.begin(req)
	if err != nil || outcome != rollbackBeginReplay {
		t.Fatalf("replay begin: outcome=%v err=%v", outcome, err)
	}
	if replay.GetMatrixRunId() != "mx_20260910_8f31" || replay.GetStatus() != "success" {
		t.Fatalf("replayed result: %+v", replay)
	}
}

func TestMatrixLedger_DifferentPayloadSameRequestID(t *testing.T) {
	l := newTestLedger(t)
	req := matrixReq("req-1", MatrixCommandBuild)
	if _, _, err := l.begin(req); err != nil {
		t.Fatal(err)
	}
	if err := l.complete("req-1", successMatrixResult(req, "mx_20260910_0001")); err != nil {
		t.Fatal(err)
	}

	// Same request_id, different payload → ALREADY_EXISTS (backend bug, not a
	// silent second execution).
	changed := matrixReq("req-1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
		r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.23"}}
	})
	if _, _, err := l.begin(changed); err == nil {
		t.Fatal("begin with different payload must fail")
	}
}

func TestMatrixLedger_RestartInProgressBecomesIndeterminate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-matrix-ledger.json")
	l := newRollbackLedger(path, rollbackLedgerCapacity, "matrix", "any matrix run the command created is persisted")
	if err := l.initialize(); err != nil {
		t.Fatal(err)
	}
	req := matrixReq("req-crash", MatrixCommandBuild)
	if _, _, err := l.begin(req); err != nil {
		t.Fatal(err)
	}

	// Simulate the agent restart: a fresh ledger instance over the same file
	// converts the in-progress entry to a terminal indeterminate result and
	// queues it for replay.
	l2 := newRollbackLedger(path, rollbackLedgerCapacity, "matrix", "any matrix run the command created is persisted")
	if err := l2.initialize(); err != nil {
		t.Fatal(err)
	}
	pending, err := l2.pendingResults()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending results = %d, want 1", len(pending))
	}
	res := pending[0]
	if res.GetStatus() != "error" || res.GetErrorCode() != "UNAVAILABLE" {
		t.Fatalf("indeterminate result: %+v", res)
	}
	if res.GetCommand() != MatrixCommandBuild {
		t.Fatalf("indeterminate result command = %q, want %q", res.GetCommand(), MatrixCommandBuild)
	}
	if res.GetError() == "" || !contains(res.GetError(), "matrix run") {
		t.Fatalf("indeterminate result must carry the matrix follow-up note: %q", res.GetError())
	}

	// The restarted ledger must refuse to re-execute the request.
	outcome, replay, err := l2.begin(req)
	if err != nil {
		t.Fatalf("begin after restart: %v", err)
	}
	if outcome != rollbackBeginReplay || replay.GetStatus() != "error" {
		t.Fatalf("begin after restart must replay the indeterminate result, got outcome=%v replay=%+v", outcome, replay)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestMatrixLedger_PersistsCommandType(t *testing.T) {
	l := newTestLedger(t)
	req := matrixReq("req-p", MatrixCommandDockerize)
	if _, _, err := l.begin(req); err != nil {
		t.Fatal(err)
	}
	if err := l.complete(req.GetRequestId(), successMatrixResult(req, "")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	var disk rollbackLedgerFileData
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk.Entries) != 1 || disk.Entries[0].Command != MatrixCommandDockerize {
		t.Fatalf("persisted entry: %+v", disk.Entries)
	}
}

// --- Handler execution -------------------------------------------------------

func TestExecuteMatrixCommand_NoHandlerRegistered(t *testing.T) {
	orig := getMatrixHandler()
	SetMatrixHandler(nil)
	defer SetMatrixHandler(orig)

	req := matrixReq("r1", MatrixCommandBuild)
	res := executeMatrixCommand(req)
	if res.GetStatus() != "error" || res.GetErrorCode() != "UNIMPLEMENTED" {
		t.Fatalf("result: %+v", res)
	}
	if res.GetRequestId() != "r1" || res.GetCommand() != MatrixCommandBuild {
		t.Fatalf("identity fields: %+v", res)
	}
}

func TestExecuteMatrixCommand_PanicBecomesTerminalError(t *testing.T) {
	orig := getMatrixHandler()
	SetMatrixHandler(func(*pb.MonitorCommandRequest) *pb.MonitorCommandResult { panic("boom") })
	defer SetMatrixHandler(orig)

	res := executeMatrixCommand(matrixReq("r1", MatrixCommandBuild))
	if res.GetStatus() != "error" || res.GetErrorCode() == "" {
		t.Fatalf("panic result: %+v", res)
	}
}

func TestExecuteMatrixCommand_FinalizesResult(t *testing.T) {
	orig := getMatrixHandler()
	defer SetMatrixHandler(orig)

	// Handler returning a bare error result gets identity fields stamped and
	// a default error code; success results pass through untouched.
	SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		return &pb.MonitorCommandResult{Status: "error", Error: "bad thing"}
	})
	res := executeMatrixCommand(matrixReq("r9", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) { r.AppName = "app" }))
	if res.GetRequestId() != "r9" || res.GetCommand() != MatrixCommandBuild || res.GetAppName() != "app" {
		t.Fatalf("identity: %+v", res)
	}
	if res.GetErrorCode() != "UNKNOWN" || res.GetTimestamp() == 0 {
		t.Fatalf("defaults: %+v", res)
	}

	SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		return &pb.MonitorCommandResult{Status: "success", MatrixRunId: "mx_20260910_8f31", MatrixRun: &pb.MatrixRunState{MatrixRunId: "mx_20260910_8f31"}}
	})
	res = executeMatrixCommand(matrixReq("r10", MatrixCommandBuild))
	if res.GetStatus() != "success" || res.GetMatrixRun().GetMatrixRunId() != "mx_20260910_8f31" {
		t.Fatalf("success passthrough: %+v", res)
	}
}

// --- End-to-end over the MonitorStream ---------------------------------------

// waitForMatrixResultStatus waits for the first command result of requestID
// carrying the wanted status. Results accumulate in the fake backend's
// history (duplicates, replays), so matching on status is what distinguishes
// the duplicate's UNAVAILABLE from the execution's terminal result.
func waitForMatrixResultStatus(b *fakeMonitorBackend, requestID, status string) *pb.MonitorCommandResult {
	deadline := time.After(5 * time.Second)
	for {
		b.mu.Lock()
		for _, ev := range b.received {
			if res, ok := ev.Payload.(*pb.MonitorEvent_CommandResult); ok &&
				res.CommandResult.GetRequestId() == requestID && res.CommandResult.GetStatus() == status {
				b.mu.Unlock()
				return res.CommandResult
			}
		}
		b.mu.Unlock()
		select {
		case <-b.newEvent:
		case <-deadline:
			return nil
		}
	}
}

// startMatrixTestStream opens a monitor stream against the fake backend and
// returns a stop function. The ServerInfo wait doubles as the
// "stream established" barrier.
func startMatrixTestStream(t *testing.T, b *fakeMonitorBackend, c *Client) (stop func()) {
	t.Helper()
	streamErrCh := make(chan error, 1)
	go func() { streamErrCh <- c.runMonitorStream() }()
	if ev := b.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for initial ServerInfo event")
	}
	return func() {
		close(c.done)
		select {
		case <-streamErrCh:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for monitor stream to shut down")
		}
	}
}

func TestMonitorStream_MatrixCommandAsyncAndIdempotent(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	// The matrix ledger must be anchored at the isolated data dir for this
	// test, and re-initialized so in-progress conversion can't leak across.
	origLedger := matrixResults
	matrixResults = newRollbackLedger(filepath.Join(t.TempDir(), "matrix-ledger.json"), rollbackLedgerCapacity, "matrix", "test note")
	defer func() { matrixResults = origLedger }()

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 500 * time.Millisecond
	defer func() { monitorMetricsInterval = origInterval }()

	// The test asserts that ServerMetrics keeps flowing while a matrix
	// command is held, and server metrics ride the server tick, so that
	// cadence must be shortened too.
	origServerInterval := serverMetricsInterval
	serverMetricsInterval = 500 * time.Millisecond
	defer func() { serverMetricsInterval = origServerInterval }()

	origCollector := monitorStream.metricsCollector
	monitorStream.metricsCollector = fakeMetricsCollector{}
	defer func() { monitorStream.metricsCollector = origCollector }()

	var calls atomic.Int64
	release := make(chan struct{})
	origHandler := getMatrixHandler()
	SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		calls.Add(1)
		<-release // hold execution until the test lets it finish
		return successMatrixResult(req, "mx_20260910_aa01")
	})
	defer SetMatrixHandler(origHandler)

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()
	stop := startMatrixTestStream(t, backend, c)
	defer stop()

	send := func(req *pb.MonitorCommandRequest) {
		backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: req}}
	}

	// Mutating command executes asynchronously: no result before release.
	send(matrixReq("req-1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
		r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}, Platforms: []string{"linux/amd64"}}
	}))
	time.Sleep(150 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	// A duplicate while in progress is answered UNAVAILABLE synchronously.
	send(matrixReq("req-1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
		r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}, Platforms: []string{"linux/amd64"}}
	}))
	dup := waitForMatrixResultStatus(backend, "req-1", "error")
	if dup == nil || dup.GetErrorCode() != "UNAVAILABLE" {
		t.Fatalf("duplicate result: %+v", dup)
	}

	// Metrics keep flowing while the build is held (async execution): the
	// paused-metrics behavior of lifecycle commands must not apply here.
	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerMetrics)
		return ok
	}, 3*time.Second); ev == nil {
		t.Fatal("metrics were paused during a matrix command — they must keep flowing")
	}

	close(release)
	res := waitForMatrixResultStatus(backend, "req-1", "success")
	if res == nil {
		t.Fatal("timed out waiting for the matrix build result")
	}
	// The held execution finished; the duplicate already returned UNAVAILABLE,
	// so exactly one handler invocation has run to completion.
	if res.GetMatrixRunId() != "mx_20260910_aa01" {
		t.Fatalf("build result: %+v", res)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler ran %d times for one request, want 1", got)
	}

	// A retry of the SAME request (backend timeout + resend) replays the
	// stored result without re-executing.
	send(matrixReq("req-1", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
		r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}, Platforms: []string{"linux/amd64"}}
	}))
	// Give the replay a moment to arrive, then verify the handler was never
	// re-invoked and the stored result round-trips.
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler re-executed on replay (%d calls)", got)
	}
	if pending, err := matrixResults.pendingResults(); err != nil || len(pending) != 0 {
		t.Fatalf("replayed result must be marked delivered, pending=%d err=%v", len(pending), err)
	}
}

func TestMonitorStream_MatrixStatusSynchronous(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	origLedger := matrixResults
	matrixResults = newRollbackLedger(filepath.Join(t.TempDir(), "matrix-ledger.json"), rollbackLedgerCapacity, "matrix", "test note")
	defer func() { matrixResults = origLedger }()

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 500 * time.Millisecond
	defer func() { monitorMetricsInterval = origInterval }()

	origCollector := monitorStream.metricsCollector
	monitorStream.metricsCollector = fakeMetricsCollector{}
	defer func() { monitorStream.metricsCollector = origCollector }()

	origHandler := getMatrixHandler()
	SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		if req.GetType() != MatrixCommandStatus {
			t.Errorf("unexpected command %q", req.GetType())
		}
		return &pb.MonitorCommandResult{
			Status:      "success",
			MatrixRunId: "mx_20260910_bb02",
			MatrixRun: &pb.MatrixRunState{
				MatrixRunId: "mx_20260910_bb02",
				AppName:     req.GetAppName(),
				Status:      "succeeded",
				Counters:    &pb.MatrixCounters{Total: 2, Succeeded: 2},
			},
		}
	})
	defer SetMatrixHandler(origHandler)

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()
	stop := startMatrixTestStream(t, backend, c)
	defer stop()

	backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{
		Command: matrixReq("req-s", MatrixCommandStatus),
	}}
	res := waitForMatrixResultStatus(backend, "req-s", "success")
	if res == nil {
		t.Fatal("timed out waiting for the status result")
	}
	if res.GetStatus() != "success" || res.GetMatrixRun().GetMatrixRunId() != "mx_20260910_bb02" {
		t.Fatalf("status result: %+v", res)
	}
	if res.GetMatrixRun().GetCounters().GetTotal() != 2 {
		t.Fatalf("status counters: %+v", res.GetMatrixRun().GetCounters())
	}
}

func TestMonitorStream_MatrixValidationErrors(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	origLedger := matrixResults
	matrixResults = newRollbackLedger(filepath.Join(t.TempDir(), "matrix-ledger.json"), rollbackLedgerCapacity, "matrix", "test note")
	defer func() { matrixResults = origLedger }()

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 500 * time.Millisecond
	defer func() { monitorMetricsInterval = origInterval }()

	origCollector := monitorStream.metricsCollector
	monitorStream.metricsCollector = fakeMetricsCollector{}
	defer func() { monitorStream.metricsCollector = origCollector }()

	// No handler needed: validation failures answer before execution.
	origHandler := getMatrixHandler()
	SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		t.Errorf("handler must not run for invalid commands")
		return &pb.MonitorCommandResult{Status: "success"}
	})
	defer SetMatrixHandler(origHandler)

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()
	stop := startMatrixTestStream(t, backend, c)
	defer stop()

	backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{
		Command: &pb.MonitorCommandRequest{RequestId: "req-bad", Type: MatrixCommandBuild, AppName: "myapp", Strategy: "rolling"},
	}}
	res := waitForMatrixResultStatus(backend, "req-bad", "error")
	if res == nil || res.GetErrorCode() != "INVALID_ARGUMENT" {
		t.Fatalf("validation result: %+v", res)
	}
}

// --- Matrix run snapshot resync ----------------------------------------------

// seedActiveMatrixRun persists a "running" run record plus a live lock (owned
// by this process) so ActiveRuns() reports it.
func seedActiveMatrixRun(t *testing.T, id string) *matrix.Run {
	t.Helper()
	run := matrix.NewRun(matrix.RunID(id), "myapp", t.TempDir(),
		&matrix.Profile{Lang: "go", Versions: []string{"1.22"}, Platforms: []string{"linux/amd64"}, Concurrency: 1},
		time.Now())
	run.InitCombinations(nil)
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	dir, err := matrix.RunsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".lock"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestMonitorStream_MatrixSnapshotResync(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 500 * time.Millisecond
	defer func() { monitorMetricsInterval = origInterval }()

	origCollector := monitorStream.metricsCollector
	monitorStream.metricsCollector = fakeMetricsCollector{}
	defer func() { monitorStream.metricsCollector = origCollector }()

	seedActiveMatrixRun(t, "mx_20260910_cc03")

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()
	stop := startMatrixTestStream(t, backend, c)
	defer stop()

	// The active run's full state arrives as a MonitorEvent matrix_run
	// snapshot right after the connection is established.
	ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		run, ok := ev.Payload.(*pb.MonitorEvent_MatrixRun)
		return ok && run.MatrixRun.GetMatrixRunId() == "mx_20260910_cc03"
	}, 3*time.Second)
	if ev == nil {
		t.Fatal("timed out waiting for matrix run snapshot resync")
	}
	state := ev.Payload.(*pb.MonitorEvent_MatrixRun).MatrixRun
	if state.GetStatus() != "running" || !state.GetActive() || state.GetLockPid() == 0 {
		t.Fatalf("snapshot state: status=%s active=%v pid=%d", state.GetStatus(), state.GetActive(), state.GetLockPid())
	}
	if state.GetConfig() == nil || state.GetConfig().GetLanguage() != "go" {
		t.Fatalf("snapshot config: %+v", state.GetConfig())
	}
}

// --- F2/F3/F6 dispatch behavior -------------------------------------------

// matrixLedgerEntryCount reports how many entries the ledger holds.
func matrixLedgerEntryCount(l *rollbackLedger) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// matrixLedgerHasEntry reports whether an entry exists for requestID.
func matrixLedgerHasEntry(l *rollbackLedger, requestID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entries[requestID] != nil
}

func waitForMatrixCount(t *testing.T, want int64, get func() int64) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for get() < want {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for count >= %d (got %d)", want, get())
		}
	}
}

// startMatrixDispatchHarness wires an isolated monitor stream for dispatch
// tests and returns the fresh matrix ledger, a send function and the fake
// backend. All teardown is registered with t.Cleanup.
func startMatrixDispatchHarness(t *testing.T) (*rollbackLedger, func(*pb.MonitorCommandRequest), *fakeMonitorBackend) {
	t.Helper()
	setupTestSession(t)
	watchOneApp(t)
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	origLedger := matrixResults
	matrixResults = newRollbackLedger(filepath.Join(t.TempDir(), "matrix-ledger.json"), rollbackLedgerCapacity, "matrix", "test note")
	t.Cleanup(func() { matrixResults = origLedger })

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 500 * time.Millisecond
	t.Cleanup(func() { monitorMetricsInterval = origInterval })

	origCollector := monitorStream.metricsCollector
	monitorStream.metricsCollector = fakeMetricsCollector{}
	t.Cleanup(func() { monitorStream.metricsCollector = origCollector })

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	t.Cleanup(cleanup)
	t.Cleanup(startMatrixTestStream(t, backend, c))

	send := func(req *pb.MonitorCommandRequest) {
		backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: req}}
	}
	return matrixResults, send, backend
}

func matrixBuildReq(id string) *pb.MonitorCommandRequest {
	return matrixReq(id, MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
		r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}, Platforms: []string{"linux/amd64"}}
	})
}

// TestMonitorStream_MatrixDryRunNoLedgerEntry (F2): a dry-run build is answered
// synchronously and takes no durable idempotency slot.
func TestMonitorStream_MatrixDryRunNoLedgerEntry(t *testing.T) {
	var calls atomic.Int64
	origHandler := getMatrixHandler()
	SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		calls.Add(1)
		return &pb.MonitorCommandResult{Status: "success", MatrixPreview: &pb.MatrixPlanPreview{Language: "go"}}
	})
	defer SetMatrixHandler(origHandler)

	ledger, send, backend := startMatrixDispatchHarness(t)

	req := matrixBuildReq("req-dry")
	req.DryRun = true
	send(req)
	if res := waitForMatrixResultStatus(backend, "req-dry", "success"); res == nil || res.GetMatrixPreview() == nil {
		t.Fatalf("dry-run result: %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1 (answered synchronously)", calls.Load())
	}
	if n := matrixLedgerEntryCount(ledger); n != 0 {
		t.Fatalf("dry-run created %d ledger entries, want 0", n)
	}
}

// TestMonitorStream_MatrixAuthorizationGate (F3): a mutating matrix command is
// refused when the authorizer denies, the refusal spends no idempotency key
// (an allowed retry of the same request_id executes), and status/dry-run are
// never routed through the gate.
func TestMonitorStream_MatrixAuthorizationGate(t *testing.T) {
	var authCalls, handlerCalls atomic.Int64
	var denyActive atomic.Bool
	denyActive.Store(true)

	origAuth := getMatrixAuthorizer()
	SetMatrixAuthorizer(func(req *pb.MonitorCommandRequest) error {
		authCalls.Add(1)
		if denyActive.Load() {
			return phelixerr.New(phelixerr.CodeAuthzDenied, "policy denies this matrix mutation")
		}
		return nil
	})
	defer SetMatrixAuthorizer(origAuth)

	origHandler := getMatrixHandler()
	SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		handlerCalls.Add(1)
		return successMatrixResult(req, "mx_20260910_dd01")
	})
	defer SetMatrixHandler(origHandler)

	ledger, send, backend := startMatrixDispatchHarness(t)

	// Denied: the authz code, no execution, no ledger entry.
	send(matrixBuildReq("req-authz"))
	if res := waitForMatrixResultStatus(backend, "req-authz", "error"); res == nil || res.GetErrorCode() != string(phelixerr.CodeAuthzDenied) {
		t.Fatalf("denied result: %+v", res)
	}
	if handlerCalls.Load() != 0 {
		t.Fatalf("denied command executed (%d handler calls)", handlerCalls.Load())
	}
	if matrixLedgerHasEntry(ledger, "req-authz") {
		t.Fatal("denied command spent an idempotency key")
	}

	// The refusal spent no key: the same request_id, once allowed, executes.
	denyActive.Store(false)
	send(matrixBuildReq("req-authz"))
	if res := waitForMatrixResultStatus(backend, "req-authz", "success"); res == nil {
		t.Fatal("allowed retry of the refused request_id did not execute")
	}
	waitForMatrixCount(t, 1, handlerCalls.Load)

	// Read-only status and dry-run never reach the authorizer.
	authBefore := authCalls.Load()
	send(matrixReq("req-st", MatrixCommandStatus))
	waitForMatrixResultStatus(backend, "req-st", "success")
	dry := matrixBuildReq("req-dry2")
	dry.DryRun = true
	send(dry)
	waitForMatrixResultStatus(backend, "req-dry2", "success")
	if authCalls.Load() != authBefore {
		t.Fatalf("authorizer consulted for a read-only/dry-run command (%d -> %d)", authBefore, authCalls.Load())
	}
}

// TestMonitorStream_MatrixConcurrencyCap (F6): concurrent mutating matrix
// executions are bounded; a command beyond the cap is answered UNAVAILABLE and
// spends no idempotency key, so it executes on retry once a slot frees.
func TestMonitorStream_MatrixConcurrencyCap(t *testing.T) {
	origSlots := matrixExecSlots
	matrixExecSlots = make(chan struct{}, 2)
	t.Cleanup(func() { matrixExecSlots = origSlots })

	var calls atomic.Int64
	release := make(chan struct{})
	origHandler := getMatrixHandler()
	SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		calls.Add(1)
		<-release
		return successMatrixResult(req, "mx_20260910_ee01")
	})
	defer SetMatrixHandler(origHandler)

	ledger, send, backend := startMatrixDispatchHarness(t)

	// Two distinct builds fill both slots and block in the handler.
	send(matrixBuildReq("req-a"))
	send(matrixBuildReq("req-b"))
	waitForMatrixCount(t, 2, calls.Load)

	// A third build has no slot: UNAVAILABLE, and it spends no key.
	send(matrixBuildReq("req-c"))
	if res := waitForMatrixResultStatus(backend, "req-c", "error"); res == nil || res.GetErrorCode() != string(phelixerr.CodeUnavailable) {
		t.Fatalf("capacity result: %+v", res)
	}
	if matrixLedgerHasEntry(ledger, "req-c") {
		t.Fatal("capacity-rejected command spent an idempotency key")
	}

	// Drain the two held builds, then the rejected request_id executes.
	close(release)
	if waitForMatrixResultStatus(backend, "req-a", "success") == nil || waitForMatrixResultStatus(backend, "req-b", "success") == nil {
		t.Fatal("held builds did not complete")
	}
	send(matrixBuildReq("req-c"))
	if waitForMatrixResultStatus(backend, "req-c", "success") == nil {
		t.Fatal("capacity-rejected request_id did not execute after a slot freed")
	}
}
