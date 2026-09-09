package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// seedRunningRun persists a run in the "running" state with one in-flight
// combination (attempt attemptNum) and one completed one, optionally holding
// the run's execution lock for the duration of the test (a live process —
// this one — owns it, which is exactly what "actively executing" means).
func seedRunningRun(t *testing.T, id string, started time.Time, lock bool, attemptNum int) *matrix.Run {
	t.Helper()
	run := matrix.NewRun(matrix.RunID(id), "app", "/tmp/p", &matrix.Profile{
		Lang: builder.Go, Versions: []string{"1.26", "1.27"},
		Platforms: []string{"linux/amd64", "linux/arm64"}, Concurrency: 2, Retries: 2,
	}, started)
	run.InitCombinations([]matrix.Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	})
	run.RecordResult(matrix.Result{
		Combination: matrix.Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success", Artifact: "/tmp/bin", SHA256: strings.Repeat("c", matrix.SHA256HexLen),
	})
	run.MarkRunning("go1.27-linux-arm64", attemptNum, time.Now())
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if lock {
		release, err := matrix.AcquireRunLock(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	return run
}

func runMatrixStatus(t *testing.T, args []string, jsonOut bool) (string, error) {
	t.Helper()
	oldJSON := matrixStatusJSON
	defer func() { matrixStatusJSON = oldJSON }()
	matrixStatusJSON = jsonOut

	var err error
	out := captureStdout(t, func() {
		err = matrixStatusCmd.RunE(matrixStatusCmd, args)
	})
	return out, err
}

func TestMatrixStatus_NoActiveRun(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	out, err := runMatrixStatus(t, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No active Matrix Run.") {
		t.Fatalf("empty status output: %q", out)
	}
}

func TestMatrixStatus_OrphanedRunningIsNotActive(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	// Persisted "running" but no live execution lock: an orphaned run (crash,
	// SIGKILL) must not be reported as active — that would fabricate state.
	seedRunningRun(t, "mx_20260910_a12f", time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC), false, 1)

	out, err := runMatrixStatus(t, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No active Matrix Run.") {
		t.Fatalf("orphaned run must not be active: %q", out)
	}
	if !strings.Contains(out, "mx_20260910_a12f") || !strings.Contains(out, "running") {
		t.Fatalf("hint about the most recent run missing: %q", out)
	}

	// Explicitly naming the orphaned run shows its state — clearly labeled.
	out, err = runMatrixStatus(t, []string{"mx_20260910_a12f"}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"orphaned", "resumable", "phelix build --matrix --resume=mx_20260910_a12f"} {
		if !strings.Contains(out, want) {
			t.Fatalf("orphaned status output missing %q:\n%s", want, out)
		}
	}
}

func TestMatrixStatus_ActiveRunShowsLiveState(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedRunningRun(t, "mx_20260910_b34c", time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC), true, 2) // in-flight attempt 2 of 3

	out, err := runMatrixStatus(t, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Matrix Run mx_20260910_b34c",
		"executing (PID",
		"go1.26-linux-amd64",
		"go1.27-linux-arm64",
		"attempt 2/3",
		"SHA256: cccccccccccc",
		"Progress: 1/2 — success 1 · failed 0 · running 1 · pending 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("active status output missing %q:\n%s", want, out)
		}
	}
}

func TestMatrixStatus_MultipleActivePicksNewestDeterministically(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedRunningRun(t, "mx_20260910_0001", time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC), true, 1)
	seedRunningRun(t, "mx_20260910_0002", time.Date(2026, 9, 10, 9, 30, 0, 0, time.UTC), true, 1) // started later → the current run

	out, err := runMatrixStatus(t, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 matrix runs are executing") {
		t.Fatalf("concurrent executions note missing: %q", out)
	}
	// The most recently started run is the one rendered; the older active
	// run is reachable only by naming it.
	if !strings.Contains(out, "Matrix Run mx_20260910_0002") {
		t.Fatalf("must show the most recently started run: %q", out)
	}
	if strings.Contains(out, "Matrix Run mx_20260910_0001") {
		t.Fatalf("the older active run must not be rendered as the current one: %q", out)
	}
}

func TestMatrixStatus_TerminalRunShowsFinalState(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260910_c56d", time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC), "success", "failed")

	out, err := runMatrixStatus(t, []string{"mx_20260910_c56d"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "partial") {
		t.Fatalf("final status missing: %q", out)
	}
	if strings.Contains(out, "executing (PID") || strings.Contains(out, "orphaned") {
		t.Fatalf("completed run must not report running state: %q", out)
	}
	if !strings.Contains(out, "retry failures with 'phelix matrix retry mx_20260910_c56d --failed'") {
		t.Fatalf("failed combinations hint missing: %q", out)
	}
}

func TestMatrixStatus_RetryRunStaysDistinct(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedRunningRun(t, "mx_20260910_0001", time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC), true, 1)

	// A manual retry of the run above: new linked run, executing now.
	source, err := matrix.LoadRun("mx_20260910_0001")
	if err != nil {
		t.Fatal(err)
	}
	retry := matrix.NewRetryRun("mx_20260910_0002", source, time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC))
	retry.InitCombinations(source.FailedCombinations())
	if err := matrix.SaveRun(retry); err != nil {
		t.Fatal(err)
	}
	release, err := matrix.AcquireRunLock(retry.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	out, err := runMatrixStatus(t, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Matrix Run mx_20260910_0002") {
		t.Fatalf("the active retry run must be shown: %q", out)
	}
	if !strings.Contains(out, "Parent Run:  mx_20260910_0001") {
		t.Fatalf("retry run must stay linked to, not merged with, its source: %q", out)
	}
}

func TestMatrixStatus_InvalidAndUnknownRunID(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	if _, err := runMatrixStatus(t, []string{"not-a-run-id"}, false); err == nil {
		t.Fatal("invalid run ID must be rejected")
	}
	if _, err := runMatrixStatus(t, []string{"mx_20260910_dead"}, false); err == nil {
		t.Fatal("unknown run ID must be rejected")
	}
}

func TestMatrixStatus_JSON(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedRunningRun(t, "mx_20260910_d78e", time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC), true, 2)

	out, err := runMatrixStatus(t, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		RunID     string `json:"run_id"`
		Status    string `json:"status"`
		Executing bool   `json:"executing"`
		PID       int    `json:"pid"`
		Counters  struct {
			Total, Succeeded, Failed, Running, Pending, Skipped int
		} `json:"counters"`
		Combinations []struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Attempts int    `json:"attempts"`
			Attempt  string `json:"attempt"`
			SHA256   string `json:"sha256"`
		} `json:"combinations"`
	}
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("JSON output invalid: %v\n%s", err, out)
	}
	if snap.RunID != "mx_20260910_d78e" || snap.Status != "running" || !snap.Executing || snap.PID == 0 {
		t.Fatalf("snapshot header: %+v", snap)
	}
	c := snap.Counters
	if c.Total != c.Succeeded+c.Failed+c.Running+c.Pending+c.Skipped || c.Total != 2 {
		t.Fatalf("JSON counters inconsistent: %+v", c)
	}
	if c.Succeeded != 1 || c.Running != 1 {
		t.Fatalf("JSON counters: %+v", c)
	}
	var running, succeeded bool
	for _, combo := range snap.Combinations {
		switch combo.Status {
		case "running":
			running = true
			if combo.Attempt != "2/3" {
				t.Fatalf("in-flight attempt = %q, want 2/3", combo.Attempt)
			}
		case "success":
			succeeded = true
			if len(combo.SHA256) != matrix.SHA256HexLen {
				t.Fatalf("full checksum missing from JSON: %q", combo.SHA256)
			}
		}
	}
	if !running || !succeeded {
		t.Fatalf("combinations missing from JSON: %+v", snap.Combinations)
	}
}

func TestMatrixStatus_JSONReleaseBlock(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	// A finished run with a release manifest: the status snapshot exposes the
	// release (version + artifacts) machine-readably.
	run := matrix.NewRun("mx_20260910_e90f", "app", "/tmp/p", &matrix.Profile{
		Lang: builder.Go, Versions: []string{"1.26"}, Platforms: []string{"linux/amd64"}, Concurrency: 2,
	}, time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC))
	run.InitCombinations([]matrix.Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	run.RecordResult(matrix.Result{
		Combination: matrix.Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success", Artifact: "/tmp/bin", SHA256: strings.Repeat("d", matrix.SHA256HexLen),
	})
	run.Finalize(time.Now())
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	manifest, err := matrix.BuildReleaseManifest(run, 7, "v1.4.0", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := matrix.SaveManifest(manifest); err != nil {
		t.Fatal(err)
	}

	out, err := runMatrixStatus(t, []string{"mx_20260910_e90f"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Release *matrix.ReleaseManifest `json:"release"`
	}
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatalf("JSON output invalid: %v\n%s", err, out)
	}
	if snap.Release == nil || snap.Release.Version != 7 || snap.Release.Status != matrix.ReleaseStatusComplete {
		t.Fatalf("release block: %+v", snap.Release)
	}
}
