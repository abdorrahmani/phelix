package matrix

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// isolateMatrixHistory points the run history at a temp dir for one test.
func isolateMatrixHistory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	return dir
}

func finishedRun(id RunID, appName string, started time.Time, statuses ...string) *Run {
	run := NewRun(id, appName, "/tmp/proj", &Profile{
		Lang: builder.Go, Versions: []string{"1.26"}, Platforms: []string{"linux/amd64"}, Concurrency: 2,
	}, started)
	run.Finish(testResults(statuses...), started.Add(time.Minute))
	return run
}

func TestSaveAndLoadRun_RoundTrip(t *testing.T) {
	isolateMatrixHistory(t)
	started := time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC)
	run := finishedRun("mx_20260909_8f31", "myapp", started, "success", "success")

	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRun("mx_20260909_8f31")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != run.ID || loaded.AppName != run.AppName || loaded.Status != RunStatusSucceeded {
		t.Fatalf("round-trip mismatch: %+v", loaded)
	}
	if len(loaded.Config.Versions) != 1 || loaded.Config.Versions[0] != "1.26" {
		t.Fatalf("config snapshot lost: %+v", loaded.Config)
	}
	if loaded.Total != 2 || loaded.Succeeded != 2 {
		t.Fatalf("counters lost: %+v", loaded)
	}
	if len(loaded.Combinations) != 2 {
		t.Fatalf("combinations lost: %+v", loaded.Combinations)
	}
}

func TestLoadRun_NotFound(t *testing.T) {
	isolateMatrixHistory(t)
	_, err := LoadRun("mx_20260909_dead")
	if phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", err)
	}
}

func TestLoadRun_RejectsTraversal(t *testing.T) {
	isolateMatrixHistory(t)
	for _, bad := range []string{"../../etc/passwd", "mx_20260909/../x"} {
		if _, err := LoadRun(RunID(bad)); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
			t.Fatalf("LoadRun(%q) = %v, want invalid-argument", bad, err)
		}
	}
}

func TestSaveRun_RejectsCollision(t *testing.T) {
	isolateMatrixHistory(t)
	run := finishedRun("mx_20260909_8f31", "app", time.Now(), "success")
	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if err := SaveRun(run); phelixerr.CodeOf(err) != phelixerr.CodeAlreadyExists {
		t.Fatalf("collision = %v, want already-exists", err)
	}
}

func TestNewUniqueRunID_AvoidsCollision(t *testing.T) {
	isolateMatrixHistory(t)

	// Deterministic entropy: the first mint collides with a persisted run,
	// the second must not — proving the retry loop actually consults the
	// history.
	if err := SaveRun(finishedRun("mx_20260909_0000", "app", time.Now(), "success")); err != nil {
		t.Fatal(err)
	}
	orig := runIDEntropy
	defer func() { runIDEntropy = orig }()
	state := 0
	runIDEntropy = func(b []byte) (int, error) {
		b[0], b[1] = 0, byte(state)
		state++
		return len(b), nil
	}

	id := NewUniqueRunID(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC))
	if id != "mx_20260909_0001" {
		t.Fatalf("unique ID = %q, want mx_20260909_0001 (first mint collides, second must not)", id)
	}
}

func TestListRuns_NewestFirst(t *testing.T) {
	isolateMatrixHistory(t)
	older := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)

	mustSave(t, finishedRun("mx_20260908_a12f", "old", older, "failed", "failed"))
	mustSave(t, finishedRun("mx_20260909_8f31", "new", newer, "success", "success"))

	runs, skipped, err := ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(runs) != 2 {
		t.Fatalf("runs=%d skipped=%d", len(runs), skipped)
	}
	if runs[0].ID != "mx_20260909_8f31" || runs[1].ID != "mx_20260908_a12f" {
		t.Fatalf("order: [%s, %s], want newest first", runs[0].ID, runs[1].ID)
	}
	if runs[0].Status != RunStatusSucceeded || runs[1].Status != RunStatusFailed {
		t.Fatalf("statuses: %s, %s", runs[0].Status, runs[1].Status)
	}
}

func TestListRuns_EmptyAndMissing(t *testing.T) {
	isolateMatrixHistory(t)
	runs, skipped, err := ListRuns()
	if err != nil || skipped != 0 || len(runs) != 0 {
		t.Fatalf("empty history: runs=%d skipped=%d err=%v", len(runs), skipped, err)
	}
}

func TestListRuns_SkipsMalformed(t *testing.T) {
	dir := isolateMatrixHistory(t)
	mustSave(t, finishedRun("mx_20260909_8f31", "good", time.Now(), "success"))

	runsDir := filepath.Join(dir, "matrix", "runs")
	if err := os.WriteFile(filepath.Join(runsDir, "mx_20260909_badc.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, "notarun.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	runs, skipped, err := ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || skipped != 1 {
		t.Fatalf("runs=%d skipped=%d, want 1/1", len(runs), skipped)
	}
	if runs[0].ID != "mx_20260909_8f31" {
		t.Fatalf("surviving run = %s", runs[0].ID)
	}
}

func TestSaveRun_NilAndInvalid(t *testing.T) {
	isolateMatrixHistory(t)
	if err := SaveRun(nil); err == nil {
		t.Fatal("nil run must be rejected")
	}
	bad := finishedRun("not-a-valid-id", "app", time.Now(), "success")
	if err := SaveRun(bad); err == nil {
		t.Fatal("invalid run ID must be rejected before touching the filesystem")
	}
}

func mustSave(t *testing.T, run *Run) {
	t.Helper()
	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}
}
