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
}

// ExecutorConfig controls worker pool and output verbosity.
type ExecutorConfig struct {
	Concurrency int
	DryRun      bool
	Debug       bool
}

// DefaultConcurrency keeps resource-heavy Docker builds conservative.
const DefaultConcurrency = 3

// Execute runs all combinations through bounded worker pool. Results retain
// plan order. Build failures do not stop unrelated combinations; callers that
// publish artifacts decide whether a partial result set is acceptable.
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

	results := make([]Result, len(plan.Combinations))
	if cfg.DryRun {
		for i, combination := range plan.Combinations {
			results[i] = Result{Combination: combination, Status: "skipped"}
		}
		return results
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

	ctx, cancel := context.WithCancel(context.Background())
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
				c := plan.Combinations[idx]
				comboID := c.ID()
				buildCtx := WithBuildProgress(ctx, comboID, func(key, stage string, current, total int64) {
					progress.Update(key, stage, current, total)
				})
				progress.Update(comboID, "starting", 0, 0)
				debugLog("START %s (GOOS=%s GOARCH=%s GOVERSION=%s)", comboID, c.OS, c.Arch, c.Version)

				start := time.Now()
				r := runBuild(buildCtx, c, fn)
				r.Duration = time.Since(start)
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
	if r.Combination == (Combination{}) {
		r.Combination = c
	}
	return r
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
