package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// ---------------------------------------------------------------------------
// Test environment: fake Go toolchain, registered app, isolated state
// ---------------------------------------------------------------------------

// fakeGoScript is a shell script standing in for the `go` binary. It reports
// a deterministic toolchain version (go1.99.0 — every test requests 1.99 so
// the native path is taken), "compiles" by writing the output binary, and can
// be told (via environment variables, which propagate through the builder's
// cmd.Env) to fail unconditionally or for one GOARCH.
const fakeGoScript = `#!/bin/sh
if [ "$1" = "env" ]; then
  echo "go1.99.0"
  exit 0
fi
if [ "$1" = "build" ]; then
  if [ -n "$FAKE_GO_FAIL" ]; then echo "fake compile error" >&2; exit 1; fi
  if [ -n "$FAKE_GO_FAIL_ARCH" ] && [ "$GOARCH" = "$FAKE_GO_FAIL_ARCH" ]; then
    echo "fake compile error for $GOARCH" >&2; exit 1
  fi
  out=""; prev=""
  for a in "$@"; do
    if [ "$prev" = "-o" ]; then out="$a"; fi
    prev="$a"
  done
  printf 'fake-binary' > "$out"
  echo "example.com/myapp"
  exit 0
fi
exit 0
`

// remoteMatrixEnv builds the isolated environment for remote matrix handler
// tests: temp HOME (deploy/version state), temp PHELIX_DATA_DIR (run
// history), a Go project directory, a registered application pointing at it,
// and a fake `go` toolchain first on PATH.
func remoteMatrixEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	projDir := t.TempDir()
	writeFile(t, filepath.Join(projDir, "go.mod"), "module example.com/myapp\n\ngo 1.99\n")
	writeFile(t, filepath.Join(projDir, "main.go"), "package main\n\nfunc main() {}\n")

	// Fake toolchain.
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "go"), fakeGoScript)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Registered application pointing at the project.
	orig := app.Manager
	am, ok := orig.(*app.AppManager)
	if !ok {
		am = &app.AppManager{}
	}
	saved := am.Apps
	am.Apps = map[string]*app.AppInfo{
		"app-1": {ID: "app-1", Name: "myapp", Directory: projDir, Language: "go"},
	}
	t.Cleanup(func() { am.Apps = saved })

	return projDir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runRemote executes one remote matrix command and returns its result.
func runRemote(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	return RemoteMatrixCommand(context.Background(), req)
}

// buildReq builds a matrix_build request for the registered app.
func buildReq(requestID string, mutators ...func(*pb.MonitorCommandRequest)) *pb.MonitorCommandRequest {
	req := &pb.MonitorCommandRequest{
		RequestId: requestID,
		Type:      phelixgrpc.MatrixCommandBuild,
		AppName:   "myapp",
		Matrix: &pb.MatrixOptions{
			GoVersions: []string{"1.99"},
			Platforms:  []string{"linux/amd64"},
		},
	}
	for _, m := range mutators {
		m(req)
	}
	return req
}

// capturedEvents records matrix events via the grpc interceptor. The
// interceptor is invoked from concurrent executor worker goroutines, so
// access is mutex-guarded.
type capturedEvents struct {
	mu     sync.Mutex
	events []*pb.MatrixEvent
}

func captureMatrixEvents(t *testing.T) *capturedEvents {
	t.Helper()
	cap := &capturedEvents{}
	phelixgrpc.SetMatrixEventInterceptor(func(ev *pb.MatrixEvent) {
		cap.mu.Lock()
		cap.events = append(cap.events, ev)
		cap.mu.Unlock()
	})
	t.Cleanup(func() { phelixgrpc.SetMatrixEventInterceptor(nil) })
	return cap
}

func (c *capturedEvents) byType(eventType string) []*pb.MatrixEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*pb.MatrixEvent
	for _, ev := range c.events {
		if ev.GetEvent() == eventType {
			out = append(out, ev)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// matrix_build
// ---------------------------------------------------------------------------

func TestRemoteMatrixBuild_DryRun(t *testing.T) {
	remoteMatrixEnv(t)

	res := runRemote(buildReq("req-dry", func(r *pb.MonitorCommandRequest) {
		r.DryRun = true
		r.Matrix.Platforms = []string{"linux/amd64", "linux/arm64"}
	}))
	if res.GetStatus() != "success" {
		t.Fatalf("status: %q error: %q", res.GetStatus(), res.GetError())
	}
	preview := res.GetMatrixPreview()
	if preview == nil {
		t.Fatal("missing matrix_preview")
	}
	if preview.GetLanguage() != "go" || preview.GetBaseCount() != 2 || len(preview.GetCombinations()) != 2 {
		t.Fatalf("preview: %+v", preview)
	}
	if preview.GetCombinations()[0] != "go1.99-linux-amd64" {
		t.Fatalf("preview combinations: %v", preview.GetCombinations())
	}
	if res.GetMatrixRunId() != "" || res.GetMatrixRun() != nil {
		t.Fatalf("dry run must not create a run: %+v", res)
	}
	// No run persisted, no events, nothing executed.
	if runs, _, err := matrix.ListRuns(); err != nil || len(runs) != 0 {
		t.Fatalf("dry run persisted runs: %d (err %v)", len(runs), err)
	}
}

func TestRemoteMatrixBuild_Success(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	events := captureMatrixEvents(t)

	res := runRemote(buildReq("req-ok", func(r *pb.MonitorCommandRequest) {
		r.Matrix.Platforms = []string{"linux/amd64", "linux/arm64"}
		r.Matrix.Tag = "v1"
	}))
	if res.GetStatus() != "success" {
		t.Fatalf("status: %q error: %q (code %s)", res.GetStatus(), res.GetError(), res.GetErrorCode())
	}

	runID := res.GetMatrixRunId()
	if runID == "" {
		t.Fatal("result missing matrix_run_id")
	}
	if strings.HasPrefix(runID, "mx_") != true {
		t.Fatalf("run id format: %s", runID)
	}
	state := res.GetMatrixRun()
	if state == nil {
		t.Fatal("result missing matrix_run state")
	}
	if state.GetStatus() != "succeeded" || state.GetCounters().GetSucceeded() != 2 {
		t.Fatalf("state: %s counters=%+v", state.GetStatus(), state.GetCounters())
	}
	if state.GetAppName() != "myapp" || state.GetProjectDir() != projDir {
		t.Fatalf("state identity: %+v", state)
	}
	cfg := state.GetConfig()
	if cfg.GetLanguage() != "go" || len(cfg.GetVersions()) != 1 || cfg.GetVersions()[0] != "1.99" ||
		len(cfg.GetPlatforms()) != 2 {
		t.Fatalf("state config: %+v", cfg)
	}
	if cfg.GetSources().GetVersions() != matrix.SourceCLI {
		t.Fatalf("payload dimensions must be recorded as source cli: %+v", cfg.GetSources())
	}

	for _, combo := range state.GetCombinations() {
		if combo.GetStatus() != "success" || combo.GetArtifact() == "" || len(combo.GetSha256()) != 64 {
			t.Fatalf("combination: %+v", combo)
		}
		if combo.GetSizeBytes() != int64(len("fake-binary")) {
			t.Fatalf("size: %d", combo.GetSizeBytes())
		}
		if combo.GetIdentity() != runID+"/"+combo.GetId() {
			t.Fatalf("identity: %s", combo.GetIdentity())
		}
	}
	// A successful run gains a release manifest view (complete).
	if rel := state.GetRelease(); rel == nil || rel.GetStatus() != "complete" || rel.GetArtifacts() != 2 {
		t.Fatalf("release: %+v", rel)
	}

	// The run is persisted in the local history with the same outcome.
	run, err := matrix.LoadRun(matrix.RunID(runID))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != matrix.RunStatusSucceeded || run.Total != 2 {
		t.Fatalf("persisted run: %s total=%d", run.Status, run.Total)
	}
	if run.Config.BuildArgs != nil {
		t.Fatalf("unexpected build args: %v", run.Config.BuildArgs)
	}

	// Event stream: started → per-combination started/completed → completed,
	// correlated by request id and run id, strictly increasing seq.
	if got := len(events.byType(phelixgrpc.MatrixEventStarted)); got != 1 {
		t.Fatalf("started events: %d", got)
	}
	if got := len(events.byType(phelixgrpc.MatrixEventCombinationStarted)); got != 2 {
		t.Fatalf("combination_started events: %d", got)
	}
	if got := len(events.byType(phelixgrpc.MatrixEventCombinationCompleted)); got != 2 {
		t.Fatalf("combination_completed events: %d", got)
	}
	if got := len(events.byType(phelixgrpc.MatrixEventCompleted)); got != 1 {
		t.Fatalf("completed events: %d", got)
	}
	var lastSeq int64
	for _, ev := range events.events {
		if ev.GetRequestId() != "req-ok" || ev.GetMatrixRunId() != runID || ev.GetMode() != "native" {
			t.Fatalf("event correlation: %+v", ev)
		}
		if ev.GetSeq() <= lastSeq {
			t.Fatalf("seq not increasing: %d after %d", ev.GetSeq(), lastSeq)
		}
		lastSeq = ev.GetSeq()
	}
	for _, ev := range events.byType(phelixgrpc.MatrixEventCombinationCompleted) {
		c := ev.GetCombination()
		if c.GetStatus() != "success" || c.GetSha256() == "" || c.GetArtifact() == "" {
			t.Fatalf("combination event payload: %+v", c)
		}
	}
}

func TestRemoteMatrixBuild_PartialFailure(t *testing.T) {
	remoteMatrixEnv(t)
	events := captureMatrixEvents(t)
	t.Setenv("FAKE_GO_FAIL_ARCH", "arm64")

	res := runRemote(buildReq("req-partial", func(r *pb.MonitorCommandRequest) {
		r.Matrix.Platforms = []string{"linux/amd64", "linux/arm64"}
	}))
	// Partial failure: the CLI's non-zero-exit semantics (BUILD_FAILED) with
	// the full state attached — one success, one failure.
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeBuildFailed) {
		t.Fatalf("status: %q code: %q error: %q", res.GetStatus(), res.GetErrorCode(), res.GetError())
	}
	state := res.GetMatrixRun()
	if state == nil || state.GetStatus() != "partial" {
		t.Fatalf("state: %+v", state)
	}
	if state.GetCounters().GetSucceeded() != 1 || state.GetCounters().GetFailed() != 1 {
		t.Fatalf("counters: %+v", state.GetCounters())
	}
	var failedSeen bool
	for _, combo := range state.GetCombinations() {
		if combo.GetStatus() == "failed" {
			failedSeen = true
			if combo.GetError() == "" || combo.GetSha256() != "" {
				t.Fatalf("failed combination: %+v", combo)
			}
			if len(combo.GetAttemptLog()) != 1 {
				t.Fatalf("attempt log: %+v", combo.GetAttemptLog())
			}
		}
	}
	if !failedSeen {
		t.Fatal("no failed combination in the result state")
	}
	if got := len(events.byType(phelixgrpc.MatrixEventCompleted)); got != 1 {
		t.Fatalf("terminal events: %d", got)
	}
	terminal := events.byType(phelixgrpc.MatrixEventCompleted)[0]
	if terminal.GetStatus() != "partial" {
		t.Fatalf("terminal event status: %s", terminal.GetStatus())
	}
	// A partial run still forms a (partial) release of its successful set.
	if rel := state.GetRelease(); rel == nil || rel.GetStatus() != "partial" || rel.GetArtifacts() != 1 {
		t.Fatalf("release: %+v", rel)
	}
}

func TestRemoteMatrixBuild_AppNotFound(t *testing.T) {
	remoteMatrixEnv(t)

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-404",
		Type:      phelixgrpc.MatrixCommandBuild,
		AppName:   "ghost",
		Matrix:    &pb.MatrixOptions{GoVersions: []string{"1.99"}, Platforms: []string{"linux/amd64"}},
	})
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeNotFound) {
		t.Fatalf("result: %+v", res)
	}
}

func TestRemoteMatrixBuild_UsesAppYAMLProfile(t *testing.T) {
	projDir := remoteMatrixEnv(t)

	// The app's phelix.yaml carries an enabled matrix profile; the payload
	// sends no dimensions — the profile must be honored, exactly like a local
	// `phelix build` in that directory.
	writeFile(t, filepath.Join(projDir, "phelix.yaml"), `
matrix:
  enabled: true
  go:
    versions: ["1.99"]
  platforms: [linux/amd64, linux/arm64]
`)

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-yaml",
		Type:      phelixgrpc.MatrixCommandBuild,
		AppName:   "myapp",
		Matrix:    &pb.MatrixOptions{},
	})
	if res.GetStatus() != "success" {
		t.Fatalf("status: %q error: %q", res.GetStatus(), res.GetError())
	}
	cfg := res.GetMatrixRun().GetConfig()
	if len(cfg.GetVersions()) != 1 || len(cfg.GetPlatforms()) != 2 {
		t.Fatalf("yaml profile not honored: %+v", cfg)
	}
	if cfg.GetSources().GetVersions() != matrix.SourceConfig {
		t.Fatalf("versions source: %+v", cfg.GetSources())
	}
}

// ---------------------------------------------------------------------------
// matrix_resume
// ---------------------------------------------------------------------------

// seedInterruptedRun persists an interrupted run: one succeeded combination
// (with a real artifact on disk) and one still pending.
func seedInterruptedRun(t *testing.T, projDir, id string) *matrix.Run {
	t.Helper()
	prof := &matrix.Profile{
		Lang:        builder.Go,
		Versions:    []string{"1.99"},
		Platforms:   []string{"linux/amd64", "linux/arm64"},
		Concurrency: 1,
		Source:      matrix.ProfileSource{Lang: matrix.SourceCLI, Versions: matrix.SourceCLI, Platforms: matrix.SourceCLI, Concurrency: matrix.SourceCLI},
	}
	run := matrix.NewRun(matrix.RunID(id), "myapp", projDir, prof, time.Now())
	combos := []matrix.Combination{
		{Lang: builder.Go, Version: "1.99", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.99", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	}
	run.InitCombinations(combos)

	artifact := filepath.Join(projDir, "builds", "matrix", combos[0].ID(), combos[0].BinaryName("myapp"))
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, artifact, "previous-binary")
	sum, err := matrix.SHA256File(artifact)
	if err != nil {
		t.Fatal(err)
	}
	run.RecordResult(matrix.Result{
		Combination: combos[0],
		Status:      "success",
		Artifact:    artifact,
		SHA256:      sum,
		Attempts:    []matrix.Attempt{{Number: 1, Status: "success"}},
	})
	run.Finalize(time.Now()) // one incomplete → interrupted
	if run.Status != matrix.RunStatusInterrupted {
		t.Fatalf("fixture status: %s", run.Status)
	}
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRemoteMatrixResume_ExecutesIncomplete(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	events := captureMatrixEvents(t)
	seedInterruptedRun(t, projDir, "mx_20260910_aa10")

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-res",
		Type:      phelixgrpc.MatrixCommandResume,
		AppName:   "myapp",
		Target:    "mx_20260910_aa10",
	})
	if res.GetStatus() != "success" {
		t.Fatalf("status: %q error: %q (code %s)", res.GetStatus(), res.GetError(), res.GetErrorCode())
	}
	if res.GetMatrixRunId() != "mx_20260910_aa10" {
		t.Fatalf("resume must keep the same run id: %s", res.GetMatrixRunId())
	}
	state := res.GetMatrixRun()
	if state.GetStatus() != "succeeded" || state.GetCounters().GetSucceeded() != 2 {
		t.Fatalf("state: %s counters=%+v", state.GetStatus(), state.GetCounters())
	}
	if state.GetResumeCount() != 1 {
		t.Fatalf("resume count: %d", state.GetResumeCount())
	}

	// The previously-succeeded combination kept its original artifact and
	// checksum; the resumed one was built now.
	var kept, rebuilt bool
	for _, c := range state.GetCombinations() {
		if c.GetId() == "go1.99-linux-amd64" && c.GetSizeBytes() == int64(len("previous-binary")) {
			kept = true
		}
		if c.GetId() == "go1.99-linux-arm64" && c.GetSizeBytes() == int64(len("fake-binary")) {
			rebuilt = true
		}
	}
	if !kept || !rebuilt {
		t.Fatalf("kept=%v rebuilt=%v combos=%+v", kept, rebuilt, state.GetCombinations())
	}

	if got := len(events.byType(phelixgrpc.MatrixEventResumed)); got != 1 {
		t.Fatalf("resumed events: %d", got)
	}
	// Only the incomplete combination executed.
	if got := len(events.byType(phelixgrpc.MatrixEventCombinationStarted)); got != 1 {
		t.Fatalf("combination_started events: %d, want 1 (completed combos never rebuild)", got)
	}
	if got := len(events.byType(phelixgrpc.MatrixEventCompleted)); got != 1 {
		t.Fatalf("terminal events: %d", got)
	}
}

func TestRemoteMatrixResume_DryRun(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	seedInterruptedRun(t, projDir, "mx_20260910_aa11")

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-res-dry",
		Type:      phelixgrpc.MatrixCommandResume,
		Target:    "mx_20260910_aa11",
		DryRun:    true,
	})
	if res.GetStatus() != "success" {
		t.Fatalf("status: %q error: %q", res.GetStatus(), res.GetError())
	}
	preview := res.GetMatrixPreview()
	if preview == nil {
		t.Fatal("missing preview")
	}
	if preview.GetResumeRunId() != "mx_20260910_aa11" || preview.GetRunTotal() != 2 {
		t.Fatalf("preview: %+v", preview)
	}
	if len(preview.GetExecuteIds()) != 1 || preview.GetExecuteIds()[0] != "go1.99-linux-arm64" {
		t.Fatalf("execute ids: %v", preview.GetExecuteIds())
	}
	// The run record is untouched: no resume count bump, still interrupted.
	run, err := matrix.LoadRun("mx_20260910_aa11")
	if err != nil {
		t.Fatal(err)
	}
	if run.ResumeCount != 0 || run.Status != matrix.RunStatusInterrupted {
		t.Fatalf("dry run mutated the run: count=%d status=%s", run.ResumeCount, run.Status)
	}
}

func TestRemoteMatrixResume_LatestAndGuards(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	seedInterruptedRun(t, projDir, "mx_20260910_aa12")

	// target=latest finds the resumable run; an explicit app_name that
	// disagrees is rejected.
	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-res-latest",
		Type:      phelixgrpc.MatrixCommandResume,
		Target:    "latest",
	})
	if res.GetStatus() != "success" || res.GetMatrixRunId() != "mx_20260910_aa12" {
		t.Fatalf("latest resume: %+v", res)
	}

	res = runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-res-wrongapp",
		Type:      phelixgrpc.MatrixCommandResume,
		AppName:   "otherapp",
		Target:    "mx_20260910_aa12",
	})
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("wrong-app resume: %+v", res)
	}
}

func TestRemoteMatrixResume_NotResumable(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	run := seedInterruptedRun(t, projDir, "mx_20260910_aa13")
	// Make the run terminal (nothing incomplete).
	for i := range run.Combinations {
		run.Combinations[i].Status = "success"
	}
	run.Finalize(time.Now())
	if err := matrix.UpdateRun(run); err != nil {
		t.Fatal(err)
	}

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-res-done",
		Type:      phelixgrpc.MatrixCommandResume,
		Target:    "mx_20260910_aa13",
	})
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("resume of a finished run: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// matrix_retry
// ---------------------------------------------------------------------------

// seedFailedRun persists a run whose every combination failed.
func seedFailedRun(t *testing.T, projDir, id string) *matrix.Run {
	t.Helper()
	prof := &matrix.Profile{
		Lang:        builder.Go,
		Versions:    []string{"1.99"},
		Platforms:   []string{"linux/amd64"},
		Concurrency: 1,
		Source:      matrix.ProfileSource{Lang: matrix.SourceCLI, Versions: matrix.SourceCLI, Platforms: matrix.SourceCLI, Concurrency: matrix.SourceCLI},
	}
	run := matrix.NewRun(matrix.RunID(id), "myapp", projDir, prof, time.Now())
	combos := []matrix.Combination{{Lang: builder.Go, Version: "1.99", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}}
	run.InitCombinations(combos)
	run.RecordResult(matrix.Result{
		Combination: combos[0],
		Status:      "failed",
		Error:       phelixerr.New(phelixerr.CodeBuildFailed, "go build failed"),
		Attempts:    []matrix.Attempt{{Number: 1, Status: "failed"}},
	})
	run.Finalize(time.Now())
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRemoteMatrixRetry_NewLinkedRun(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	events := captureMatrixEvents(t)
	source := seedFailedRun(t, projDir, "mx_20260910_bb20")

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-retry",
		Type:      phelixgrpc.MatrixCommandRetry,
		Target:    "mx_20260910_bb20",
	})
	if res.GetStatus() != "success" {
		t.Fatalf("status: %q error: %q (code %s)", res.GetStatus(), res.GetError(), res.GetErrorCode())
	}
	newID := res.GetMatrixRunId()
	if newID == "" || newID == "mx_20260910_bb20" {
		t.Fatalf("retry must create a new run: %q", newID)
	}
	state := res.GetMatrixRun()
	if state.GetStatus() != "succeeded" || state.GetParentRunId() != "mx_20260910_bb20" {
		t.Fatalf("state: %s parent=%s", state.GetStatus(), state.GetParentRunId())
	}

	// The source run is immutable history.
	after, err := matrix.LoadRun("mx_20260910_bb20")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != source.Status || !after.FinishedAt.Equal(source.FinishedAt) || after.Succeeded != source.Succeeded {
		t.Fatalf("source run was modified: %+v", after)
	}

	if got := len(events.byType(phelixgrpc.MatrixEventStarted)); got != 1 {
		t.Fatalf("started events: %d", got)
	}
	if events.byType(phelixgrpc.MatrixEventStarted)[0].GetMatrixRunId() != newID {
		t.Fatal("retry events must carry the NEW run id")
	}
}

func TestRemoteMatrixRetry_NoFailures(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	seedInterruptedRun(t, projDir, "mx_20260910_bb21") // has a success

	// Flip everything to success so nothing is failed.
	run, err := matrix.LoadRun("mx_20260910_bb21")
	if err != nil {
		t.Fatal(err)
	}
	for i := range run.Combinations {
		run.Combinations[i].Status = "success"
	}
	run.Finalize(time.Now())
	if err := matrix.UpdateRun(run); err != nil {
		t.Fatal(err)
	}

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-retry-none",
		Type:      phelixgrpc.MatrixCommandRetry,
		Target:    "mx_20260910_bb21",
	})
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("retry without failures: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// matrix_status / matrix_list
// ---------------------------------------------------------------------------

func TestRemoteMatrixStatus(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	seedInterruptedRun(t, projDir, "mx_20260910_cc30")
	// Make it a live run: persisted status "running" plus this process holding
	// the execution lock — exactly what a build in progress looks like.
	run, err := matrix.LoadRun("mx_20260910_cc30")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = matrix.RunStatusRunning
	if err := matrix.UpdateRun(run); err != nil {
		t.Fatal(err)
	}
	release, err := matrix.AcquireRunLock("mx_20260910_cc30")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	statusReq := func(requestID, target, app string) *pb.MonitorCommandResult {
		return runRemote(&pb.MonitorCommandRequest{
			RequestId: requestID,
			Type:      phelixgrpc.MatrixCommandStatus,
			AppName:   app,
			Target:    target,
		})
	}

	// Active (default target).
	res := statusReq("req-st-1", "", "")
	if res.GetStatus() != "success" || res.GetMatrixRunId() != "mx_20260910_cc30" {
		t.Fatalf("active status: %+v", res)
	}
	if !res.GetMatrixRun().GetActive() || res.GetMatrixRun().GetLockPid() == 0 {
		t.Fatalf("active flags: %+v", res.GetMatrixRun())
	}
	if res.GetMatrixRun().GetStatus() != "running" {
		t.Fatalf("persisted status: %s", res.GetMatrixRun().GetStatus())
	}

	// Explicit ID.
	res = statusReq("req-st-2", "mx_20260910_cc30", "")
	if res.GetStatus() != "success" || res.GetMatrixRun().GetCounters().GetTotal() != 2 {
		t.Fatalf("explicit status: %+v", res)
	}

	// Explicit ID with a mismatching app guard.
	res = statusReq("req-st-3", "mx_20260910_cc30", "otherapp")
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("mismatched app: %+v", res)
	}

	// Unknown run.
	res = statusReq("req-st-4", "mx_20260910_ffff", "")
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeNotFound) {
		t.Fatalf("unknown run: %+v", res)
	}
}

func TestRemoteMatrixStatus_NoActiveRun(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	seedFailedRun(t, projDir, "mx_20260910_cc31") // terminal, not active

	// No active run: success with matrix_run unset and the most recent
	// summary carried in matrix_runs — never a fabricated run state.
	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-st-none",
		Type:      phelixgrpc.MatrixCommandStatus,
	})
	if res.GetStatus() != "success" {
		t.Fatalf("status: %q error: %q", res.GetStatus(), res.GetError())
	}
	if res.GetMatrixRun() != nil || res.GetMatrixRunId() != "" {
		t.Fatalf("no active run must not fabricate state: %+v", res)
	}
	if len(res.GetMatrixRuns()) != 1 || res.GetMatrixRuns()[0].GetMatrixRunId() != "mx_20260910_cc31" {
		t.Fatalf("most recent summary: %+v", res.GetMatrixRuns())
	}

	// target=latest returns that run's full state.
	res = runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-st-latest",
		Type:      phelixgrpc.MatrixCommandStatus,
		Target:    "latest",
	})
	if res.GetStatus() != "success" || res.GetMatrixRunId() != "mx_20260910_cc31" {
		t.Fatalf("latest status: %+v", res)
	}
	if res.GetMatrixRun().GetStatus() != "failed" || res.GetMatrixRun().GetActive() {
		t.Fatalf("latest state: %+v", res.GetMatrixRun())
	}
}

func TestRemoteMatrixList(t *testing.T) {
	projDir := remoteMatrixEnv(t)
	seedFailedRun(t, projDir, "mx_20260910_dd40")
	seedInterruptedRun(t, projDir, "mx_20260910_dd41")

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-list",
		Type:      phelixgrpc.MatrixCommandList,
		Matrix:    &pb.MatrixOptions{ListLimit: 1},
	})
	if res.GetStatus() != "success" || len(res.GetMatrixRuns()) != 1 {
		t.Fatalf("list: %+v", res)
	}
	if res.GetMatrixRuns()[0].GetMatrixRunId() != "mx_20260910_dd41" {
		t.Fatalf("list must be newest first: %+v", res.GetMatrixRuns())
	}

	// App filter excludes everything for a foreign app.
	res = runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-list-foreign",
		Type:      phelixgrpc.MatrixCommandList,
		AppName:   "otherapp",
	})
	if res.GetStatus() != "success" || len(res.GetMatrixRuns()) != 0 {
		t.Fatalf("filtered list: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// matrix_dockerize
// ---------------------------------------------------------------------------

func TestRemoteMatrixDockerize_DryRun(t *testing.T) {
	remoteMatrixEnv(t)

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-dk-dry",
		Type:      phelixgrpc.MatrixCommandDockerize,
		AppName:   "myapp",
		DryRun:    true,
		Matrix: &pb.MatrixOptions{
			GoVersions: []string{"1.99"},
			Platforms:  []string{"linux/amd64", "linux/arm64"},
			Tag:        "v2",
		},
	})
	if res.GetStatus() != "success" {
		t.Fatalf("status: %q error: %q", res.GetStatus(), res.GetError())
	}
	if preview := res.GetMatrixPreview(); preview == nil || preview.GetBaseCount() != 2 {
		t.Fatalf("preview: %+v", res.GetMatrixPreview())
	}
	if res.GetMatrixDocker() != nil {
		t.Fatalf("dry run must not report a docker outcome: %+v", res.GetMatrixDocker())
	}
}

func TestRemoteMatrixDockerize_MultiArchRequiresPush(t *testing.T) {
	remoteMatrixEnv(t)

	res := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-dk-ma",
		Type:      phelixgrpc.MatrixCommandDockerize,
		AppName:   "myapp",
		DryRun:    true, // validation fires before the daemon check
		Matrix: &pb.MatrixOptions{
			GoVersions:   []string{"1.99"},
			Platforms:    []string{"linux/amd64"},
			MultiArchTag: true,
			Push:         false,
		},
	})
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeInvalidArgument) {
		t.Fatalf("multi-arch without push: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Cross-cutting: interrupted remote session (context cancellation)
// ---------------------------------------------------------------------------

func TestRemoteMatrixBuild_ContextCancellationInterruptsRun(t *testing.T) {
	remoteMatrixEnv(t)

	// A context canceled before execution: no combinations start, the run
	// finalizes interrupted, and the terminal result carries the resumable
	// state (the daemon-shutdown semantics).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := buildReq("req-cancel")
	req.Matrix.Platforms = []string{"linux/amd64"}
	res := RemoteMatrixCommand(ctx, req)
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeBuildFailed) {
		t.Fatalf("result: %+v", res)
	}
	state := res.GetMatrixRun()
	if state == nil || state.GetStatus() != "interrupted" {
		t.Fatalf("state: %+v", state)
	}
	if state.GetCounters().GetPending() != 1 {
		t.Fatalf("counters: %+v", state.GetCounters())
	}

	// The interrupted run is resumable and the resume completes it.
	resume := runRemote(&pb.MonitorCommandRequest{
		RequestId: "req-cancel-resume",
		Type:      phelixgrpc.MatrixCommandResume,
		Target:    res.GetMatrixRunId(),
	})
	if resume.GetStatus() != "success" || resume.GetMatrixRun().GetStatus() != "succeeded" {
		t.Fatalf("resume after cancellation: %+v", resume)
	}
}
