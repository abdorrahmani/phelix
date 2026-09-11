package matrix

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// --- MarkRunning ----------------------------------------------------------------

func comboGo(id string) Combination { // convenience: identity only matters via ID()
	switch id {
	case "go1.26-linux-arm64":
		return Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"}
	default:
		return Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	}
}

func TestMarkRunning_TransitionsAndCounters(t *testing.T) {
	run := testRun(t)
	run.InitCombinations([]Combination{comboGo("go1.26-linux-amd64"), comboGo("go1.26-linux-arm64")})

	c := countersMustSum(t, run)
	if c.Pending != 2 || c.Running != 0 {
		t.Fatalf("fresh run counters: %+v", c)
	}

	started := time.Now()
	run.MarkRunning("go1.26-linux-amd64", 1, started)

	c = countersMustSum(t, run)
	if c.Pending != 1 || c.Running != 1 {
		t.Fatalf("after MarkRunning: %+v", c)
	}
	rc := run.Combinations[0]
	if rc.Status != "running" || rc.Attempts != 1 || !rc.StartedAt.Equal(started) {
		t.Fatalf("running entry incomplete: %+v", rc)
	}
	// A pending/running entry keeps the run resumable.
	if !run.Resumable() {
		t.Fatal("running combination must keep the run resumable")
	}

	// A second attempt (automatic retry in flight) updates the attempt number.
	run.MarkRunning("go1.26-linux-amd64", 2, started.Add(time.Second))
	if rc := run.Combinations[0]; rc.Attempts != 2 {
		t.Fatalf("in-flight attempt not recorded: %+v", rc)
	}

	// Recording the final result overwrites the running mark.
	run.RecordResult(Result{Combination: comboGo("go1.26-linux-amd64"), Status: "success", Artifact: "/bin", SHA256: "ab",
		Attempts: []Attempt{{Number: 1, Status: "failed"}, {Number: 2, Status: "success"}}})
	rc = run.Combinations[0]
	if rc.Status != "success" || rc.SHA256 != "ab" || rc.Attempts != 2 || rc.StartedAt.IsZero() {
		t.Fatalf("final result must replace the running mark (keeping StartedAt): %+v", rc)
	}
	c = countersMustSum(t, run)
	if c.Succeeded != 1 || c.Running != 0 || c.Pending != 1 {
		t.Fatalf("after final result: %+v", c)
	}
}

func TestMarkRunning_IgnoresUnknownAndTerminal(t *testing.T) {
	run := testRun(t)
	run.InitCombinations([]Combination{comboGo("go1.26-linux-amd64")})
	run.RecordResult(Result{Combination: comboGo("go1.26-linux-amd64"), Status: "success", Artifact: "/bin", SHA256: "ab"})

	// A terminal entry must never be flipped back to running.
	run.MarkRunning("go1.26-linux-amd64", 1, time.Now())
	if rc := run.Combinations[0]; rc.Status != "success" || rc.SHA256 != "ab" {
		t.Fatalf("terminal entry was rewritten: %+v", rc)
	}
	// Unknown IDs are ignored.
	run.MarkRunning("go9.99-windows-foo", 1, time.Now())
	if len(run.Combinations) != 1 {
		t.Fatalf("unknown combination was created: %+v", run.Combinations)
	}
}

func TestMarkRunning_ClearsStaleOutcomeOnReexecution(t *testing.T) {
	// Defensive path: an incomplete entry that somehow carries outcome data
	// (e.g. a re-executed orphaned-running combination) must not keep a stale
	// artifact or checksum from an earlier attempt.
	run := testRun(t)
	run.InitCombinations([]Combination{comboGo("go1.26-linux-amd64")})
	run.Combinations[0].Status = "running"
	run.Combinations[0].Artifact = "/stale/bin"
	run.Combinations[0].SHA256 = "stale"

	run.MarkRunning("go1.26-linux-amd64", 1, time.Now())
	rc := run.Combinations[0]
	if rc.Artifact != "" || rc.SHA256 != "" {
		t.Fatalf("stale outcome survived re-execution: %+v", rc)
	}
}

// --- SnapshotCounters -------------------------------------------------------------

func countersMustSum(t *testing.T, run *Run) StatusCounters {
	t.Helper()
	c := run.SnapshotCounters()
	if c.Total != c.Succeeded+c.Failed+c.Skipped+c.Running+c.Pending {
		t.Fatalf("inconsistent counters: %+v (sum mismatch)", c)
	}
	if c.Total != len(run.Combinations) {
		t.Fatalf("counters disagree with combination entries: %+v", c)
	}
	return c
}

func TestSnapshotCounters_MixedStates(t *testing.T) {
	run := testRun(t)
	run.InitCombinations([]Combination{comboGo("go1.26-linux-amd64"), comboGo("go1.26-linux-arm64")})
	run.RecordResult(Result{Combination: comboGo("go1.26-linux-amd64"), Status: "success"})
	run.RecordResult(Result{Combination: comboGo("go1.26-linux-arm64"), Status: "failed"})
	run.InitCombinations([]Combination{
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	})
	run.MarkRunning("go1.27-linux-amd64", 1, time.Now())

	c := countersMustSum(t, run)
	if c.Total != 4 || c.Succeeded != 1 || c.Failed != 1 || c.Running != 1 || c.Pending != 1 {
		t.Fatalf("mixed-state counters: %+v", c)
	}
	if c.Complete() != 2 {
		t.Fatalf("Complete() = %d, want 2", c.Complete())
	}
}

// --- Clone ------------------------------------------------------------------------

func TestClone_IsDeepAndRaceSafe(t *testing.T) {
	run := testRun(t)
	run.InitCombinations([]Combination{comboGo("go1.26-linux-amd64"), comboGo("go1.26-linux-arm64")})
	run.RecordResult(Result{Combination: comboGo("go1.26-linux-amd64"), Status: "success", Artifact: "/bin", SHA256: "ab"})

	clone := run.Clone()
	// Mutating the original's slices/maps must not affect the clone.
	run.Combinations[0].Status = "failed"
	run.Combinations[1].Metadata = map[string]string{"k": "v"}
	if clone.Combinations[0].Status != "success" {
		t.Fatalf("clone shares combination storage with the original: %+v", clone.Combinations[0])
	}
	if clone.Combinations[1].Metadata != nil {
		t.Fatalf("clone shares metadata storage with the original: %+v", clone.Combinations[1])
	}
	// After restoring the original, a fresh clone serializes identically to
	// the original — the clone is a faithful value snapshot.
	run.Combinations[0].Status = "success"
	run.Combinations[1].Metadata = nil
	original, _ := json.Marshal(run)
	twin, _ := json.Marshal(run.Clone())
	if string(original) != string(twin) {
		t.Fatalf("clone roundtrip mismatch:\n%s\n---\n%s", original, twin)
	}
}

func TestClone_ConcurrentWithMutations(t *testing.T) {
	// The persistence path marshals clones while workers mutate the run;
	// this test is meaningful under -race.
	run := testRun(t)
	combs := make([]Combination, 0, 8)
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		combs = append(combs, Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: p})
	}
	run.InitCombinations(combs)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				run.MarkRunning(combs[(i+j)%len(combs)].ID(), 1, time.Now())
				_ = run.Clone()
			}
		}(i)
	}
	wg.Wait()
}

// --- Executor OnAttempt -------------------------------------------------------------

func TestExecute_OnAttemptFiresPerAttempt(t *testing.T) {
	plan, err := ParsePlan(builder.Go, []string{"1.26"}, []string{"linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	calls := make(map[string][]int)
	var mu sync.Mutex
	attempts := 0
	fn := func(_ context.Context, c Combination) *Result {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts <= 2 {
			return &Result{Combination: c, Status: "failed", Error: transientErr}
		}
		return &Result{Combination: c, Status: "success", Artifact: "/bin"}
	}
	results := Execute(plan, fn, ExecutorConfig{
		Concurrency: 1,
		Retries:     2,
		OnAttempt: func(c Combination, attempt int) {
			mu.Lock()
			defer mu.Unlock()
			calls[c.ID()] = append(calls[c.ID()], attempt)
		},
	})
	if len(results) != 1 || results[0].Status != "success" || len(results[0].Attempts) != 3 {
		t.Fatalf("results: %+v", results)
	}
	got := calls[plan.Combinations[0].ID()]
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("OnAttempt sequence = %v, want [1 2 3]", got)
	}
}

func TestExecute_OnAttemptOptional(t *testing.T) {
	plan, err := ParsePlan(builder.Go, []string{"1.26"}, []string{"linux/amd64"})
	if err != nil {
		t.Fatal(err)
	}
	fn := func(_ context.Context, c Combination) *Result {
		return &Result{Combination: c, Status: "success", Artifact: "/bin"}
	}
	results := Execute(plan, fn, ExecutorConfig{Concurrency: 1}) // no OnAttempt: must not panic
	if len(results) != 1 || results[0].Status != "success" {
		t.Fatalf("results: %+v", results)
	}
}
