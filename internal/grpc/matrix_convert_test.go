package grpc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// testRun builds a fully-populated run record for conversion tests.
func testRun(t *testing.T) *matrix.Run {
	t.Helper()
	prof := &matrix.Profile{
		Lang:        builder.Go,
		Versions:    []string{"1.22", "1.23"},
		Platforms:   []string{"linux/amd64", "linux/arm64"},
		Concurrency: 2,
		Retries:     1,
		Include:     []matrix.Rule{{Dimensions: matrix.Dimensions{"lang": "go", "version": "1.24", "os": "linux", "arch": "amd64"}, Metadata: map[string]string{"tag": "latest"}}},
		Source: matrix.ProfileSource{
			Lang:        matrix.SourceCLI,
			Versions:    matrix.SourceCLI,
			Platforms:   matrix.SourceConfig,
			Concurrency: matrix.SourceDefault,
			Include:     matrix.SourceConfig,
		},
	}
	run := matrix.NewRun(matrix.RunID("mx_20260910_8f31"), "myapp", "/tmp/proj", prof, time.Now())
	combos := []matrix.Combination{
		{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.22", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	}
	run.InitCombinations(combos)

	// Artifact for the first combination so size/sha256/conversion paths are
	// exercised with real bytes on disk.
	artifact := filepath.Join(t.TempDir(), "myapp_amd64_go_1.22")
	if err := os.WriteFile(artifact, []byte("fake binary bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := matrix.SHA256File(artifact)
	if err != nil {
		t.Fatal(err)
	}
	run.RecordResult(matrix.Result{
		Combination: combos[0],
		Status:      "success",
		Duration:    1500 * time.Millisecond,
		Artifact:    artifact,
		SHA256:      sum,
		CacheStatus: "cold",
		Attempts: []matrix.Attempt{
			{Number: 1, Status: "success", Duration: 1500 * time.Millisecond},
		},
	})
	run.RecordResult(matrix.Result{
		Combination: combos[1],
		Status:      "failed",
		Duration:    300 * time.Millisecond,
		Error:       errBuild("go build failed"),
		Attempts: []matrix.Attempt{
			{Number: 1, Status: "failed", Duration: 300 * time.Millisecond, Error: errBuild("go build failed")},
		},
	})
	run.Finalize(time.Now())
	return run
}

type errBuild string

func (e errBuild) Error() string { return string(e) }

func TestToProtoMatrixRunState(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	run := testRun(t)

	state := ToProtoMatrixRunState(run)
	if state.GetMatrixRunId() != "mx_20260910_8f31" || state.GetAppName() != "myapp" || state.GetProjectDir() != "/tmp/proj" {
		t.Fatalf("identity: %+v", state)
	}
	if state.GetStatus() != string(matrix.RunStatusPartial) {
		t.Fatalf("status = %s, want partial", state.GetStatus())
	}

	cfg := state.GetConfig()
	if cfg.GetLanguage() != "go" || len(cfg.GetVersions()) != 2 || len(cfg.GetPlatforms()) != 2 || cfg.GetConcurrency() != 2 || cfg.GetRetries() != 1 {
		t.Fatalf("config: %+v", cfg)
	}
	if len(cfg.GetInclude()) != 1 || cfg.GetInclude()[0].GetMetadata()["tag"] != "latest" {
		t.Fatalf("include rules: %+v", cfg.GetInclude())
	}
	if cfg.GetSources() == nil || cfg.GetSources().GetLanguage() == "" {
		t.Fatalf("sources: %+v", cfg.GetSources())
	}

	counters := state.GetCounters()
	if counters.GetTotal() != 2 || counters.GetSucceeded() != 1 || counters.GetFailed() != 1 {
		t.Fatalf("counters: %+v", counters)
	}
	if counters.GetTotal() != counters.GetSucceeded()+counters.GetFailed()+counters.GetSkipped()+counters.GetRunning()+counters.GetPending() {
		t.Fatalf("counter invariant broken: %+v", counters)
	}

	if len(state.GetCombinations()) != 2 {
		t.Fatalf("combinations: %d", len(state.GetCombinations()))
	}
	success := state.GetCombinations()[0]
	if success.GetId() != "go1.22-linux-amd64" || success.GetIdentity() != "mx_20260910_8f31/go1.22-linux-amd64" {
		t.Fatalf("success combo identity: %+v", success)
	}
	if success.GetStatus() != "success" || success.GetArtifact() == "" || len(success.GetSha256()) != 64 {
		t.Fatalf("success combo outcome: %+v", success)
	}
	if success.GetSizeBytes() != int64(len("fake binary bytes")) {
		t.Fatalf("size = %d", success.GetSizeBytes())
	}
	if success.GetDurationMs() != 1500 || success.GetAttempts() != 1 || success.GetCacheStatus() != "cold" {
		t.Fatalf("success combo details: %+v", success)
	}
	if len(success.GetAttemptLog()) != 1 || success.GetAttemptLog()[0].GetNumber() != 1 {
		t.Fatalf("attempt log: %+v", success.GetAttemptLog())
	}

	failed := state.GetCombinations()[1]
	if failed.GetStatus() != "failed" || failed.GetError() == "" || failed.GetSha256() != "" || failed.GetSizeBytes() != 0 {
		t.Fatalf("failed combo: %+v", failed)
	}
	if state.GetFinishedAt() == 0 || state.GetDurationMs() < 0 {
		t.Fatalf("timing: finished=%d duration=%d", state.GetFinishedAt(), state.GetDurationMs())
	}
	// No lock is held and no manifest exists for this fabricated run.
	if state.GetActive() || state.GetLockPid() != 0 || state.GetRelease() != nil {
		t.Fatalf("lock/manifest: active=%v pid=%d release=%+v", state.GetActive(), state.GetLockPid(), state.GetRelease())
	}
}

func TestToProtoMatrixRunState_ManifestEnrichment(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	run := testRun(t)
	run.Status = matrix.RunStatusPartial
	manifest := &matrix.ReleaseManifest{
		SchemaVersion:     1,
		AppName:           "myapp",
		Version:           7,
		Tag:               "v7",
		MatrixRunID:       run.ID,
		CreatedAt:         time.Now(),
		Status:            matrix.ReleaseStatusPartial,
		TotalCombinations: 2,
	}
	if err := matrix.SaveManifest(manifest); err != nil {
		t.Fatal(err)
	}

	state := ToProtoMatrixRunState(run)
	rel := state.GetRelease()
	if rel == nil {
		t.Fatal("release info missing")
	}
	if rel.GetVersion() != 7 || rel.GetTag() != "v7" || rel.GetStatus() != "partial" || rel.GetArtifacts() != 0 || rel.GetTotalCombinations() != 2 {
		t.Fatalf("release: %+v", rel)
	}
}

func TestToProtoMatrixRunState_ActiveLock(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	run := testRun(t)
	run.Status = matrix.RunStatusRunning

	release, err := matrix.AcquireRunLock(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	state := ToProtoMatrixRunState(run)
	if !state.GetActive() || state.GetLockPid() == 0 {
		t.Fatalf("active lock not reported: active=%v pid=%d", state.GetActive(), state.GetLockPid())
	}
}

func TestToProtoMatrixRunSummary(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	run := testRun(t)
	run.ResumeCount = 2
	run.ParentRunID = matrix.RunID("mx_20260909_0001")

	s := ToProtoMatrixRunSummary(run)
	if s.GetMatrixRunId() != "mx_20260910_8f31" || s.GetParentRunId() != "mx_20260909_0001" || s.GetResumeCount() != 2 {
		t.Fatalf("summary: %+v", s)
	}
	if s.GetCounters().GetTotal() != 2 || s.GetStartedAt() == 0 {
		t.Fatalf("summary details: %+v", s)
	}
}

func TestToProtoMatrixPlanPreview(t *testing.T) {
	plan, err := matrix.ParsePlan(builder.Go, []string{"1.22", "1.23"}, []string{"linux/amd64", "linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	preview := ToProtoMatrixPlanPreview(plan)
	if preview.GetLanguage() != "go" || preview.GetBaseCount() != 4 || len(preview.GetCombinations()) != 4 {
		t.Fatalf("preview: %+v", preview)
	}
	if preview.GetCombinations()[0] != "go1.22-linux-amd64" {
		t.Fatalf("preview order: %v", preview.GetCombinations())
	}
	if ToProtoMatrixPlanPreview(nil) != nil {
		t.Fatal("nil plan must convert to nil preview")
	}
}

func TestDurationMS(t *testing.T) {
	if got := durationMS("1.5s"); got != 1500 {
		t.Fatalf("durationMS(1.5s) = %d", got)
	}
	if got := durationMS(""); got != 0 {
		t.Fatalf("durationMS(empty) = %d", got)
	}
	if got := durationMS("not-a-duration"); got != 0 {
		t.Fatalf("durationMS(garbage) = %d", got)
	}
}

func TestToProtoMatrixCombinationFromResult(t *testing.T) {
	artifact := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(artifact, []byte("artifact"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := matrix.SHA256File(artifact)
	if err != nil {
		t.Fatal(err)
	}
	res := matrix.Result{
		Combination: matrix.Combination{Lang: builder.Rust, Version: "1.77", OS: "linux", Arch: "arm", Variant: "v7", Platform: "linux/arm/v7", Metadata: map[string]string{"tag": "latest"}},
		Status:      "success",
		Duration:    2 * time.Second,
		Artifact:    artifact,
		SHA256:      sum,
		CacheStatus: "hit",
		Attempts: []matrix.Attempt{
			{Number: 1, Status: "failed", Duration: time.Second, Error: errBuild("net timeout")},
			{Number: 2, Status: "success", Duration: time.Second},
		},
	}
	c := ToProtoMatrixCombinationFromResult(res)
	if c.GetId() != "rust1.77-linux-arm-v7" || c.GetToolchain() != "rust" || c.GetVariant() != "v7" {
		t.Fatalf("identity: %+v", c)
	}
	if c.GetDurationMs() != 2000 || c.GetSizeBytes() != int64(len("artifact")) || c.GetAttempts() != 2 {
		t.Fatalf("details: %+v", c)
	}
	if len(c.GetAttemptLog()) != 2 || c.GetAttemptLog()[0].GetError() == "" {
		t.Fatalf("attempt log: %+v", c.GetAttemptLog())
	}
	if c.GetMetadata()["tag"] != "latest" {
		t.Fatalf("metadata: %+v", c.GetMetadata())
	}
	// A zero result with a status defaults to one attempt (matches the run
	// record convention).
	res.Attempts = nil
	if got := ToProtoMatrixCombinationFromResult(res).GetAttempts(); got != 1 {
		t.Fatalf("default attempts = %d, want 1", got)
	}
}
