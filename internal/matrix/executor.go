package matrix

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// BuildFunc is signature function builds one combination.
type BuildFunc func(ctx context.Context, c Combination) *Result

// Attempt is one execution attempt of one combination. Attempt 1 is the
// original execution; automatic retries append attempts 2, 3, … for the same
// combination inside the same Matrix Run.
type Attempt struct {
	Number   int
	Status   string
	Duration time.Duration
	Error    error
}

// Result describes outcome building one combination.
type Result struct {
	Combination Combination
	Status      string
	Duration    time.Duration
	Artifact    string
	Error       error
	Log         string
	// CacheStatus is the compiler-cache classification for this combination
	// ("cold" / "hit"), derived from toolchain output by the builders. Empty
	// means unknown. Consumed by the build-report integration.
	CacheStatus string
	// Attempts records every execution attempt in order (automatic retries
	// included). The final attempt's status equals Status; earlier attempts
	// are history and are never overwritten.
	Attempts []Attempt
}

// ExecutorConfig controls worker pool, output verbosity, and retry policy.
type ExecutorConfig struct {
	Concurrency int
	DryRun      bool
	Debug       bool
	// Retries is the automatic-retry budget: failed combinations are retried
	// up to Retries additional times (so Retries=2 allows at most 3 attempts).
	Retries int
	// Classifies whether a failure is worth retrying. nil means
	// DefaultRetryClassifier. Non-retryable failures fail the combination on
	// the first attempt — deterministic configuration/compiler errors must
	// not be repeated.
	Classifier RetryClassifier
	// Context, when non-nil, is the parent of the execution context. Workers
	// stop scheduling new combinations once it is canceled (signal handling,
	// resume safety). In-flight builds receive the cancellation through their
	// build context and their results are discarded — the combinations stay
	// pending in the run so a resume executes them.
	Context context.Context
	// OnResult, when non-nil, is invoked once per combination after its final
	// result is known (all attempts done). It is called from worker
	// goroutines, so implementations must be safe for concurrent use; it is
	// never called for discarded (canceled mid-build) results.
	OnResult func(Result)
}

// DefaultConcurrency keeps resource-heavy Docker builds conservative.
const DefaultConcurrency = 3

// RetryClassifier decides whether a failed attempt should be retried.
type RetryClassifier func(err error) bool

// DefaultRetryClassifier retries only failures whose error code marks them as
// plausibly transient (network, connection, timeout, unavailable, Docker
// daemon). Everything else — build failures (compiler errors), configuration,
// validation, unsupported targets — is treated as permanent: retrying a
// deterministic failure would only burn the retry budget and produce the same
// result.
//
// Limitation (documented): Docker *pull* failures and Docker *build* failures
// share CodeDocker/CodeBuildFailed wrapping at the cmd layer, so they are not
// retryable under this classifier; only daemon unavailability is.
func DefaultRetryClassifier(err error) bool {
	switch phelixerr.CodeOf(err) {
	case phelixerr.CodeNetwork, phelixerr.CodeConnection, phelixerr.CodeTimeout,
		phelixerr.CodeUnavailable, phelixerr.CodeDockerDaemonUnavailable:
		return true
	}
	return false
}

// Execute runs all combinations through bounded worker pool, applying the
// configured automatic-retry policy to failed combinations. Results retain
// plan order; combinations that were never started (canceled execution) keep
// the zero Result (empty Status) — callers recording incrementally via
// OnResult see those combinations stay pending. Build failures do not stop
// unrelated combinations; callers that publish artifacts decide whether a
// partial result set is acceptable.
//
// A build function that panics or returns nil is isolated: that combination
// is recorded as failed and the remaining combinations still run to
// completion — one broken combination must never take down the whole matrix.
func Execute(plan *MatrixPlan, fn BuildFunc, cfg ExecutorConfig) []Result {
	if plan == nil || len(plan.Combinations) == 0 {
		return nil
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	classifier := cfg.Classifier
	if classifier == nil {
		classifier = DefaultRetryClassifier
	}

	results := make([]Result, len(plan.Combinations))
	if cfg.DryRun {
		for i, combination := range plan.Combinations {
			results[i] = Result{Combination: combination, Status: "skipped"}
		}
		return results
	}

	parent := cfg.Context
	if parent == nil {
		parent = context.Background()
	}

	var mu sync.Mutex
	progress := NewMatrixProgress(plan.Combinations, os.Stdout)
	if !cfg.Debug {
		progress.Start()
	}
	debugLog := func(format string, args ...any) {
		if cfg.Debug {
			fmt.Printf(" %s %s\n", color.New(color.Faint).Sprint("[debug]"), fmt.Sprintf(format, args...))
		}
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	ch := make(chan int, len(plan.Combinations))
	for i := range plan.Combinations {
		ch <- i
	}
	close(ch)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				// Stop scheduling new combinations once the parent context is
				// canceled (interrupted run); already-started builds finish or
				// are killed via the build context below.
				if ctx.Err() != nil {
					return
				}
				c := plan.Combinations[idx]
				comboID := c.ID()
				buildCtx := WithBuildProgress(ctx, comboID, func(key, stage string, current, total int64) {
					progress.Update(key, stage, current, total)
				})
				progress.Update(comboID, "starting", 0, 0)
				debugLog("START %s (GOOS=%s GOARCH=%s GOVERSION=%s)", comboID, c.OS, c.Arch, c.Version)

				r := executeWithRetries(ctx, buildCtx, c, fn, cfg.Retries, classifier, progress, debugLog)

				// A canceled context invalidates in-flight results: the build
				// was killed mid-way, not failed. Discard them so the
				// combination stays pending (resumable) and no partial
				// artifact is mistaken for a final one.
				if ctx.Err() != nil && r.Status == "failed" {
					debugLog("DISCARDED %s (execution canceled mid-build)", comboID)
					continue
				}

				if r.Status == "failed" {
					progress.Finish(comboID, "failed")
					debugLog("FAILED %s in %s: %v", comboID, r.Duration.Round(time.Millisecond), r.Error)
				} else {
					progress.Finish(comboID, "done")
					debugLog("completed %s in %s → %s", comboID, r.Duration.Round(time.Millisecond), r.Artifact)
				}

				if cfg.Debug && r.Log != "" {
					fmt.Printf(" %s build log for %s:\n", color.New(color.Faint).Sprint("[debug]"), comboID)
					for _, line := range strings.Split(r.Log, "\n") {
						if line != "" {
							fmt.Printf(" %s\n", line)
						}
					}
				}

				mu.Lock()
				results[idx] = *r
				mu.Unlock()
				if cfg.OnResult != nil {
					cfg.OnResult(*r)
				}
			}
		}()
	}

	wg.Wait()
	cancel()
	if !cfg.Debug {
		progress.Stop()
	}
	return results
}

// executeWithRetries runs one combination under the automatic-retry policy:
// attempt 1 plus up to retries additional attempts, but only while the
// failure is classified retryable. Successful combinations never retry. Each
// attempt is recorded in the result's Attempts history.
func executeWithRetries(ctx, buildCtx context.Context, c Combination, fn BuildFunc, retries int, classifier RetryClassifier, progress *MatrixProgress, debugLog func(string, ...any)) *Result {
	maxAttempts := 1 + retries
	// runBuild returns a fresh Result per attempt, so the attempt history is
	// accumulated here — never overwritten, never lost on the last attempt.
	var history []Attempt
	var r *Result
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		start := time.Now()
		r = runBuild(buildCtx, c, fn)
		duration := time.Since(start)
		r.Duration = duration
		history = append(history, Attempt{
			Number:   attempt,
			Status:   r.Status,
			Duration: duration,
			Error:    r.Error,
		})
		r.Attempts = history

		if r.Status != "failed" {
			return r
		}
		if attempt == maxAttempts {
			return r
		}
		if !classifier(r.Error) {
			debugLog("NOT RETRYING %s: failure is not transient (%v)", c.ID(), r.Error)
			return r
		}
		stage := fmt.Sprintf("retry %d/%d", attempt+1, maxAttempts)
		progress.Update(c.ID(), stage, 0, 0)
		debugLog("RETRY %s: attempt %d/%d after failure: %v", c.ID(), attempt+1, maxAttempts, r.Error)
	}
	return r
}

// runBuild invokes fn with panic and nil-result isolation, always returning a
// result attributed to the requested combination.
func runBuild(ctx context.Context, c Combination, fn BuildFunc) (r *Result) {
	defer func() {
		if rec := recover(); rec != nil {
			r = &Result{
				Combination: c,
				Status:      "failed",
				Error:       phelixerr.Newf(phelixerr.CodeBuildFailed, "matrix build panicked for %s: %v", c.ID(), rec),
			}
		}
	}()
	r = fn(ctx, c)
	if r == nil {
		r = &Result{
			Combination: c,
			Status:      "failed",
			Error:       phelixerr.Newf(phelixerr.CodeBuildFailed, "matrix build returned no result for %s", c.ID()),
		}
		return r
	}
	// A build function that forgot to attribute its result must never
	// pollute another combination's report entry.
	if isZeroCombination(r.Combination) {
		r.Combination = c
	}
	return r
}

// isZeroCombination reports whether no dimension is set. Combination carries
// a map field (Metadata) and therefore is not struct-comparable.
func isZeroCombination(c Combination) bool {
	return c.Lang == "" && c.Version == "" && c.OS == "" && c.Arch == "" &&
		c.Variant == "" && c.Platform == "" && c.Metadata == nil
}

// HasFailures reports whether any result Status is "failed".
func HasFailures(results []Result) bool {
	for _, r := range results {
		if r.Status == "failed" {
			return true
		}
	}
	return false
}

// Succeeded returns results with Status "success".
func Succeeded(results []Result) []Result {
	out := make([]Result, 0)
	for _, r := range results {
		if r.Status == "success" {
			out = append(out, r)
		}
	}
	return out
}

// Failed returns results with Status "failed".
func Failed(results []Result) []Result {
	out := make([]Result, 0)
	for _, r := range results {
		if r.Status == "failed" {
			out = append(out, r)
		}
	}
	return out
}
