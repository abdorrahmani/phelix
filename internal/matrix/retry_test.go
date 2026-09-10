package matrix

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// transientErr / permanentErr mimic the two failure families the classifier
// distinguishes.
var transientErr = phelixerr.New(phelixerr.CodeTimeout, "docker pull timed out")
var permanentErr = phelixerr.New(phelixerr.CodeBuildFailed, "compiler error")

func retryPlan() *MatrixPlan {
	return &MatrixPlan{Lang: builder.Go, Combinations: []Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	}}
}

func attemptsOf(results []Result) []int {
	out := make([]int, 0, len(results))
	for _, r := range results {
		if len(r.Attempts) == 0 {
			out = append(out, 1)
			continue
		}
		out = append(out, len(r.Attempts))
	}
	return out
}

func TestExecute_NoRetriesByDefault(t *testing.T) {
	var calls atomic.Int32
	fn := func(ctx context.Context, c Combination) *Result {
		calls.Add(1)
		return &Result{Combination: c, Status: "failed", Error: transientErr}
	}
	results := Execute(retryPlan(), fn, ExecutorConfig{Concurrency: 1, Retries: 0})
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (--matrix-retries 0 means no retry)", calls.Load())
	}
	if attemptsOf(results)[0] != 1 {
		t.Fatalf("attempts = %d", attemptsOf(results)[0])
	}
}

func TestExecute_RetriesMeansAdditionalAttempts(t *testing.T) {
	var calls atomic.Int32
	fn := func(ctx context.Context, c Combination) *Result {
		calls.Add(1)
		return &Result{Combination: c, Status: "failed", Error: transientErr}
	}
	results := Execute(retryPlan(), fn, ExecutorConfig{Concurrency: 1, Retries: 2})
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (1 + 2 retries)", calls.Load())
	}
	if got := attemptsOf(results)[0]; got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	// The attempt history must be complete and ordered.
	for i, a := range results[0].Attempts {
		if a.Number != i+1 || a.Status != "failed" {
			t.Fatalf("attempt %d = %+v", i, a)
		}
	}
}

func TestExecute_FailureThenSuccess(t *testing.T) {
	var calls atomic.Int32
	fn := func(ctx context.Context, c Combination) *Result {
		n := calls.Add(1)
		if n < 3 {
			return &Result{Combination: c, Status: "failed", Error: transientErr}
		}
		return &Result{Combination: c, Status: "success", Artifact: "/out/bin"}
	}
	results := Execute(retryPlan(), fn, ExecutorConfig{Concurrency: 1, Retries: 2})
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	if results[0].Status != "success" || results[0].Artifact != "/out/bin" {
		t.Fatalf("final result: %+v", results[0])
	}
	if len(results[0].Attempts) != 3 {
		t.Fatalf("attempt history = %d, want 3", len(results[0].Attempts))
	}
	if results[0].Attempts[0].Status != "failed" || results[0].Attempts[2].Status != "success" {
		t.Fatalf("attempt statuses: %+v", results[0].Attempts)
	}
}

func TestExecute_SuccessNeverRetried(t *testing.T) {
	var calls atomic.Int32
	fn := func(ctx context.Context, c Combination) *Result {
		calls.Add(1)
		return &Result{Combination: c, Status: "success"}
	}
	results := Execute(retryPlan(), fn, ExecutorConfig{Concurrency: 1, Retries: 5})
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (success must not be retried)", calls.Load())
	}
	if attemptsOf(results)[0] != 1 {
		t.Fatalf("attempts = %d", attemptsOf(results)[0])
	}
}

func TestExecute_PermanentFailureNotRetried(t *testing.T) {
	var calls atomic.Int32
	fn := func(ctx context.Context, c Combination) *Result {
		calls.Add(1)
		return &Result{Combination: c, Status: "failed", Error: permanentErr}
	}
	results := Execute(retryPlan(), fn, ExecutorConfig{Concurrency: 1, Retries: 3})
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (deterministic build failure must not be retried)", calls.Load())
	}
	if attemptsOf(results)[0] != 1 {
		t.Fatalf("attempts = %d", attemptsOf(results)[0])
	}
}

func TestExecute_CustomClassifier(t *testing.T) {
	var calls atomic.Int32
	fn := func(ctx context.Context, c Combination) *Result {
		calls.Add(1)
		return &Result{Combination: c, Status: "failed", Error: errors.New("custom")}
	}
	Execute(retryPlan(), fn, ExecutorConfig{
		Concurrency: 1, Retries: 1,
		Classifier: func(err error) bool { return true },
	})
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (classifier overrode the default)", calls.Load())
	}
}

// TestExecute_RetryIsolationOfCombinations is the spec's isolation scenario:
// among four combinations only the failing one may be re-executed.
func TestExecute_RetryIsolationOfCombinations(t *testing.T) {
	plan := &MatrixPlan{Lang: builder.Go, Combinations: []Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	}}
	var mu sync.Mutex
	calls := map[string]int{}
	fn := func(ctx context.Context, c Combination) *Result {
		mu.Lock()
		calls[c.ID()]++
		mu.Unlock()
		if c.ID() == "go1.27-linux-amd64" {
			return &Result{Combination: c, Status: "failed", Error: transientErr}
		}
		return &Result{Combination: c, Status: "success"}
	}
	results := Execute(plan, fn, ExecutorConfig{Concurrency: 2, Retries: 2})

	for id, n := range calls {
		want := 1
		if id == "go1.27-linux-amd64" {
			want = 3
		}
		if n != want {
			t.Fatalf("%s executed %d times, want %d", id, n, want)
		}
	}
	for i, r := range results {
		wantAttempts := 1
		if r.Combination.ID() == "go1.27-linux-amd64" {
			wantAttempts = 3
		}
		if got := attemptsOf([]Result{r})[0]; got != wantAttempts {
			t.Fatalf("result %d (%s) attempts = %d, want %d", i, r.Combination.ID(), got, wantAttempts)
		}
	}
}

func TestExecute_OnResultCalledOncePerCombination(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	fn := func(ctx context.Context, c Combination) *Result {
		if c.ID() == "go1.26-linux-amd64" {
			return &Result{Combination: c, Status: "failed", Error: transientErr}
		}
		return &Result{Combination: c, Status: "success"}
	}
	Execute(retryPlan(), fn, ExecutorConfig{
		Concurrency: 1, Retries: 2,
		OnResult: func(res Result) {
			mu.Lock()
			seen = append(seen, res.Combination.ID())
			mu.Unlock()
		},
	})
	if len(seen) != 1 {
		t.Fatalf("OnResult calls = %d (%v), want 1 (retries are not separate results)", len(seen), seen)
	}
}

func TestDefaultRetryClassifier(t *testing.T) {
	transient := []error{
		phelixerr.New(phelixerr.CodeNetwork, "n"),
		phelixerr.New(phelixerr.CodeConnection, "c"),
		phelixerr.New(phelixerr.CodeTimeout, "t"),
		phelixerr.New(phelixerr.CodeUnavailable, "u"),
		phelixerr.New(phelixerr.CodeDockerDaemonUnavailable, "d"),
	}
	for _, err := range transient {
		if !DefaultRetryClassifier(err) {
			t.Fatalf("%v should be retryable", err)
		}
	}
	permanent := []error{
		phelixerr.New(phelixerr.CodeBuildFailed, "b"),
		phelixerr.New(phelixerr.CodeInvalidArgument, "i"),
		phelixerr.New(phelixerr.CodeConfiguration, "c"),
		phelixerr.New(phelixerr.CodeValidation, "v"),
		phelixerr.New(phelixerr.CodeUnsupportedProject, "u"),
		errors.New("plain"),
		nil,
	}
	for _, err := range permanent {
		if DefaultRetryClassifier(err) {
			t.Fatalf("%v must not be retryable", err)
		}
	}
}

func TestGenerateReport_AttemptsExposed(t *testing.T) {
	results := []Result{{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success",
		Duration:    time.Second,
		Attempts: []Attempt{
			{Number: 1, Status: "failed", Duration: time.Millisecond, Error: transientErr},
			{Number: 2, Status: "success", Duration: time.Second},
		},
	}}
	report := GenerateReport("app", results, time.Now())
	cr := report.Combinations[0]
	if cr.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", cr.Attempts)
	}
	if len(cr.AttemptLog) != 2 || cr.AttemptLog[0].Status != "failed" {
		t.Fatalf("attempt log: %+v", cr.AttemptLog)
	}
	if cr.AttemptLog[0].Error == "" {
		t.Fatal("failed attempt error missing")
	}
}
