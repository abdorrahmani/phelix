package matrix

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// --- Incremental run lifecycle ----------------------------------------------

func testRun(t *testing.T) *Run {
	t.Helper()
	return NewRun("mx_20260909_8f31", "app", "/tmp/p", &Profile{
		Lang: builder.Go, Versions: []string{"1.26"}, Platforms: []string{"linux/amd64", "linux/arm64"},
	}, time.Now())
}

func TestRunLifecycle_InterruptedWhenIncomplete(t *testing.T) {
	run := testRun(t)
	run.InitCombinations([]Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	})
	if !run.Resumable() {
		t.Fatal("fresh run with pending combinations must be resumable")
	}
	run.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success",
		Attempts:    []Attempt{{Number: 1, Status: "success"}},
	})
	run.Finalize(time.Now())

	if run.Status != RunStatusInterrupted {
		t.Fatalf("status = %s, want interrupted", run.Status)
	}
	if run.Succeeded != 1 || run.Incomplete != 1 {
		t.Fatalf("counters: succeeded=%d incomplete=%d", run.Succeeded, run.Incomplete)
	}
	incomplete := run.IncompleteCombinations()
	if len(incomplete) != 1 || incomplete[0].ID() != "go1.26-linux-arm64" {
		t.Fatalf("incomplete: %+v", incomplete)
	}
}

func TestRunLifecycle_AllSucceedAfterResume(t *testing.T) {
	run := testRun(t)
	run.InitCombinations([]Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	})
	run.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success",
	})
	// Simulate resume: the remaining combination executes and succeeds.
	run.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
		Status:      "success",
	})
	run.Finalize(time.Now())

	if run.Status != RunStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", run.Status)
	}
	if run.Resumable() {
		t.Fatal("completed run must not be resumable")
	}
}

func TestRunLifecycle_InitCombinationsIdempotent(t *testing.T) {
	run := testRun(t)
	combs := []Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	}
	run.InitCombinations(combs)
	run.RecordResult(Result{Combination: combs[0], Status: "success"})
	run.InitCombinations(combs) // resume re-seeds the same plan

	if len(run.Combinations) != 1 || run.Total != 1 {
		t.Fatalf("re-seeding duplicated combinations: %+v", run.Combinations)
	}
	if run.Combinations[0].Status != "success" || run.Succeeded != 1 {
		t.Fatal("re-seeding lost the recorded outcome")
	}
}

func TestRunLifecycle_AttemptHistoryRetained(t *testing.T) {
	run := testRun(t)
	comb := Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	run.InitCombinations([]Combination{comb})
	run.RecordResult(Result{
		Combination: comb,
		Status:      "success",
		Attempts: []Attempt{
			{Number: 1, Status: "failed", Error: transientErr},
			{Number: 2, Status: "failed", Error: transientErr},
			{Number: 3, Status: "success"},
		},
	})
	entry := run.Combinations[0]
	if entry.Attempts != 3 || len(entry.AttemptLog) != 3 {
		t.Fatalf("attempts: %+v", entry)
	}
	if entry.AttemptLog[0].Status != "failed" || entry.AttemptLog[2].Status != "success" {
		t.Fatalf("attempt history overwritten: %+v", entry.AttemptLog)
	}
	if entry.Status != "success" {
		t.Fatalf("final status = %s", entry.Status)
	}
}

func TestRunLifecycle_FailedSelection(t *testing.T) {
	run := testRun(t)
	amd64 := Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	arm64 := Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"}
	run.InitCombinations([]Combination{amd64, arm64})
	run.RecordResult(Result{Combination: amd64, Status: "success"})
	run.RecordResult(Result{Combination: arm64, Status: "failed", Error: permanentErr})
	run.Finalize(time.Now())

	failed := run.FailedCombinations()
	if len(failed) != 1 || failed[0].ID() != "go1.26-linux-arm64" {
		t.Fatalf("failed selection: %+v", failed)
	}
	if run.Status != RunStatusPartial {
		t.Fatalf("status = %s, want partial", run.Status)
	}
}

func TestRunSnapshot_RecordsRulesAndRetries(t *testing.T) {
	prof := &Profile{
		Lang:      builder.Go,
		Versions:  []string{"1.26"},
		Platforms: []string{"linux/amd64"},
		Retries:   2,
		Include:   []Rule{RuleFromFields(map[string]string{"go": "1.28", "platform": "linux/amd64", "tag": "latest"})},
		Exclude:   []Rule{RuleFromFields(map[string]string{"go": "1.25"})},
	}
	run := NewRun("mx_20260909_8f31", "app", "/tmp/p", prof, time.Now())
	if run.Config.Retries != 2 || len(run.Config.Include) != 1 || len(run.Config.Exclude) != 1 {
		t.Fatalf("snapshot: %+v", run.Config)
	}
	// The reconstructed profile reproduces the same rules.
	reconstructed := run.Config.Profile()
	if len(reconstructed.Include) != 1 || reconstructed.Retries != 2 || reconstructed.Concurrency != DefaultConcurrency {
		t.Fatalf("reconstructed: %+v", reconstructed)
	}
}

// --- Manual retry runs ------------------------------------------------------

func TestNewRetryRun_LinksParentAndCopiesConfig(t *testing.T) {
	source := testRun(t)
	source.Config.Retries = 2
	source.Config.BuildArgs = []string{"-trimpath"}
	source.InitCombinations([]Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	retry := NewRetryRun("mx_20260909_b721", source, time.Now())

	if retry.ParentRunID != source.ID {
		t.Fatalf("parent = %s, want %s", retry.ParentRunID, source.ID)
	}
	if retry.AppName != source.AppName || retry.ProjectDir != source.ProjectDir {
		t.Fatalf("identity not carried over: %+v", retry)
	}
	if retry.Config.Retries != 2 || len(retry.Config.BuildArgs) != 1 {
		t.Fatalf("config not copied: %+v", retry.Config)
	}
	if len(retry.Combinations) != 0 {
		t.Fatal("retry run must start empty; the caller selects the combinations")
	}
	// The source run is untouched.
	if len(source.Combinations) != 1 || source.Status != RunStatusRunning {
		t.Fatalf("source run mutated: %+v", source)
	}
}

// --- History: incremental updates and immutability --------------------------

func TestUpdateRun_RequiresExistingRecord(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	run := testRun(t)
	if err := UpdateRun(run); phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("update of missing run = %v, want not-found", err)
	}
	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if err := UpdateRun(run); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRun_RefusesToRewriteTerminalRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	run := testRun(t)
	run.InitCombinations([]Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	run.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success",
	})
	run.Finalize(time.Now())
	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}
	// A manual retry (or anything else) must not be able to rewrite the
	// finished run's history.
	if err := UpdateRun(run); err == nil {
		t.Fatal("terminal run update accepted")
	}
}

func TestUpdateRun_PersistsIncrementalProgress(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	run := testRun(t)
	amd64 := Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	arm64 := Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"}
	run.InitCombinations([]Combination{amd64, arm64})
	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after one combination: the on-disk state must show
	// the recorded success and the pending remainder.
	run.RecordResult(Result{Combination: amd64, Status: "success"})
	if err := UpdateRun(run); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Succeeded != 1 || reloaded.Status != RunStatusRunning {
		t.Fatalf("reloaded: %+v", reloaded)
	}
	if !reloaded.Resumable() {
		t.Fatal("reloaded run must be resumable")
	}
	pending := reloaded.IncompleteCombinations()
	if len(pending) != 1 || pending[0].ID() != "go1.26-linux-arm64" {
		t.Fatalf("pending: %+v", pending)
	}
}

// --- Run locks ---------------------------------------------------------------

func TestAcquireRunLock_ExclusiveAndReleased(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	release, err := AcquireRunLock("mx_20260909_8f31")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRunLock("mx_20260909_8f31"); phelixerr.CodeOf(err) != phelixerr.CodeDeployLocked {
		t.Fatalf("second acquire = %v, want locked", err)
	}
	release()
	release2, err := AcquireRunLock("mx_20260909_8f31")
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	release2()
}

func TestAcquireRunLock_ReclaimsStaleLockAfterRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	// A lock owned by a PID that cannot exist (checked alive=false).
	stale := filepath.Join(dir, "matrix", "runs", "mx_20260909_8f31.lock")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireRunLock("mx_20260909_8f31")
	if err != nil {
		t.Fatalf("stale lock not reclaimed: %v", err)
	}
	release()
}

func TestAcquireRunLock_LiveOwnerHoldsLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	// A lock owned by this very (live) process.
	live := filepath.Join(dir, "matrix", "runs", "mx_20260909_8f31.lock")
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRunLock("mx_20260909_8f31"); phelixerr.CodeOf(err) != phelixerr.CodeDeployLocked {
		t.Fatalf("live-owner lock = %v, want locked", err)
	}
}

// --- Latest resumable run ----------------------------------------------------

func TestLatestResumableRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)

	finished := testRun(t) // mx_20260909_8f31
	finished.InitCombinations([]Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	finished.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success",
	})
	finished.Finalize(time.Now())
	if err := SaveRun(finished); err != nil {
		t.Fatal(err)
	}

	interrupted := NewRun("mx_20260909_aaaa", "otherapp", "/tmp/p", &Profile{Lang: builder.Go}, time.Now().Add(time.Minute))
	interrupted.InitCombinations([]Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	if err := SaveRun(interrupted); err != nil {
		t.Fatal(err)
	}

	// No app filter → newest resumable run wins.
	got, err := LatestResumableRun("")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != interrupted.ID {
		t.Fatalf("latest resumable = %s, want %s", got.ID, interrupted.ID)
	}
	// App filter excludes it.
	if _, err := LatestResumableRun("app"); phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("filtered lookup = %v, want not-found", err)
	}
}

// --- Concurrency: concurrent RecordResult is safe ----------------------------

func TestRunRecordResult_Concurrent(t *testing.T) {
	run := testRun(t)
	combs := make([]Combination, 0, 8)
	for _, plat := range []string{"linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64"} {
		combs = append(combs, Combination{
			Lang: builder.Go, Version: "1.26",
			OS: plat[:5], Arch: plat[6:], Platform: plat,
		})
	}
	// Fix the OS/Arch pairs properly (the slicing above is only a rough map).
	combs = combs[:0]
	for _, p := range [][3]string{
		{"linux", "amd64", "linux/amd64"}, {"linux", "arm64", "linux/arm64"},
		{"darwin", "arm64", "darwin/arm64"}, {"windows", "amd64", "windows/amd64"},
		{"linux", "arm", "linux/arm/v7"}, {"darwin", "amd64", "darwin/amd64"},
		{"linux", "386", "linux/386"}, {"windows", "arm64", "windows/arm64"},
	} {
		combs = append(combs, Combination{Lang: builder.Go, Version: "1.26", OS: p[0], Arch: p[1], Platform: p[2]})
	}
	run.InitCombinations(combs)

	var wg sync.WaitGroup
	for _, c := range combs {
		wg.Add(1)
		go func(c Combination) {
			defer wg.Done()
			run.RecordResult(Result{Combination: c, Status: "success"})
		}(c)
	}
	wg.Wait()
	run.Finalize(time.Now())

	if run.Succeeded != len(combs) || run.Status != RunStatusSucceeded {
		t.Fatalf("counters under concurrency: succeeded=%d total=%d status=%s", run.Succeeded, run.Total, run.Status)
	}
}

func TestRunRecordResult_IncompleteDecremented(t *testing.T) {
	run := testRun(t)
	amd64 := Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	arm64 := Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"}
	run.InitCombinations([]Combination{amd64, arm64})
	if run.Incomplete != 2 {
		t.Fatalf("initial incomplete = %d, want 2", run.Incomplete)
	}
	run.RecordResult(Result{Combination: amd64, Status: "success"})
	if run.Succeeded != 1 || run.Incomplete != 1 {
		t.Fatalf("after success: succeeded=%d incomplete=%d, want 1/1", run.Succeeded, run.Incomplete)
	}
	run.RecordResult(Result{Combination: arm64, Status: "failed", Error: permanentErr})
	if run.Failed != 1 || run.Incomplete != 0 {
		t.Fatalf("after failure: failed=%d incomplete=%d, want 1/0", run.Failed, run.Incomplete)
	}
}
