package matrix

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatih/color"
)

// BuildFunc is the signature of a function that builds one combination.
// It receives the combination and a context (for cancellation) and returns
// a Result describing what happened.
type BuildFunc func(ctx context.Context, c Combination) *Result

// Result describes the outcome of building one combination.
type Result struct {
	Combination Combination
	Status      string // "success", "failed", "skipped"
	Duration    time.Duration
	Artifact    string // binary path or image tag
	Error       error  // non-nil only when Status == "failed"
	Log         string // debug output captured during build
}

// ExecutorConfig controls the worker pool.
type ExecutorConfig struct {
	Concurrency int  // max parallel builds (default 3)
	DryRun      bool // if true, print the plan but don't execute
	Debug       bool // if true, print verbose build output
}

// DefaultConcurrency is the default number of parallel matrix builds.
// Each Docker-based build can be resource-heavy (CPU + memory), so we
// keep this conservative. Users can override via --matrix-concurrency.
const DefaultConcurrency = 3

// clearLine is the ANSI escape sequence to clear from cursor to end of line.
// Combined with \r (carriage return), this ensures the entire line is wiped
// before printing the new progress, preventing the garbled overlapping text
// that occurs when a shorter line follows a longer one.
const clearLine = "\r\033[K"

// Execute runs all combinations through a bounded worker pool and returns
// per-combination results in the order they were submitted (not in
// completion order).
//
// Build phase uses fail-open semantics: if one combination fails, we
// continue building the rest. This is deliberate — a matrix build is
// typically used to produce multiple artifacts in one shot, and aborting
// the entire run on the first failure would waste the work already
// completed and obscure which combinations are actually broken.
//
// Push phase (handled by the caller) uses fail-closed semantics by
// default: if any combination failed to build, the caller refuses to
// push any image. An explicit --push-partial flag overrides this. The
// rationale: publishing a partial/inconsistent release is worse than
// publishing nothing, so the safe default is to block the push.
func Execute(plan *MatrixPlan, fn BuildFunc, cfg ExecutorConfig) []Result {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}

	results := make([]Result, len(plan.Combinations))
	if cfg.DryRun {
		for i, c := range plan.Combinations {
			results[i] = Result{
				Combination: c,
				Status:      "skipped",
			}
		}
		return results
	}

	var (
		mu      sync.Mutex
		running int32
		done    int32
		total   = int32(len(plan.Combinations))
		failed  int32
		// Track which combinations are currently running for the progress display.
		activeCombos = make(map[string]time.Time) // combo ID → start time
	)

	debugLog := func(format string, args ...any) {
		if cfg.Debug {
			fmt.Printf("    %s %s\n", color.New(color.Faint).Sprint("[debug]"),
				fmt.Sprintf(format, args...))
		}
	}

	// Live progress display — shows running/done/failed counts and
	// which combinations are currently active.
	//
	// We use \033[K (ANSI "erase to end of line") after \r to ensure the
	// entire line is wiped before printing the new content. Without this,
	// shorter lines leave residual characters from longer previous lines,
	// causing the garbled overlapping output the user observed.
	//
	// In debug mode, the active combos are shown on the same line (not a
	// separate \n line) to avoid pushing the cursor down and breaking the
	// single-line progress illusion.
	progressLine := func() {
		r := atomic.LoadInt32(&running)
		d := atomic.LoadInt32(&done)
		f := atomic.LoadInt32(&failed)

		// Build the active combos list for display.
		mu.Lock()
		active := make([]string, 0, len(activeCombos))
		for id, started := range activeCombos {
			elapsed := time.Since(started).Round(time.Second)
			active = append(active, fmt.Sprintf("%s(%s)", id, elapsed))
		}
		mu.Unlock()

		// Format: "⟳ matrix build: 3/6 running, 2/6 done, 1/6 failed"
		// Use clearLine (\r\033[K) to wipe the previous line completely.
		fmt.Print(clearLine)
		fmt.Printf("  %s matrix build: %d/%d running, %d/%d done",
			color.CyanString("⟳"), r, total, d, total)
		if f > 0 {
			fmt.Printf(", %s", color.RedString("%d/%d failed", f, total))
		}
		if len(active) > 0 && cfg.Debug {
			fmt.Printf("  active: %s", strings.Join(active, ", "))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a goroutine that periodically refreshes the progress line.
	// We use a ticker instead of printing on every state change to avoid
	// terminal thrashing when many goroutines complete near-simultaneously.
	var progressDone sync.WaitGroup
	progressDone.Add(1)
	go func() {
		defer progressDone.Done()
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				progressLine()
				fmt.Println()
				return
			case <-ticker.C:
				progressLine()
			}
		}
	}()

	// Work channel: indices into plan.Combinations.
	ch := make(chan int, len(plan.Combinations))
	for i := range plan.Combinations {
		ch <- i
	}
	close(ch)

	// Launch bounded workers.
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				c := plan.Combinations[idx]
				comboID := c.ID()

				atomic.AddInt32(&running, 1)
				mu.Lock()
				activeCombos[comboID] = time.Now()
				mu.Unlock()

				debugLog("starting %s (GOOS=%s GOARCH=%s GOVERSION=%s)",
					comboID, c.OS, c.Arch, c.Version)

				start := time.Now()
				r := fn(ctx, c)
				r.Duration = time.Since(start)

				mu.Lock()
				delete(activeCombos, comboID)
				mu.Unlock()

				atomic.AddInt32(&running, -1)
				if r.Status == "failed" {
					atomic.AddInt32(&failed, 1)
					debugLog("FAILED %s in %s: %v", comboID, r.Duration.Round(time.Millisecond), r.Error)
				} else {
					debugLog("completed %s in %s → %s", comboID, r.Duration.Round(time.Millisecond), r.Artifact)
				}
				atomic.AddInt32(&done, 1)

				// In debug mode, print the captured log for this combination.
				if cfg.Debug && r.Log != "" {
					fmt.Printf("    %s build log for %s:\n", color.New(color.Faint).Sprint("[debug]"), comboID)
					for _, line := range strings.Split(r.Log, "\n") {
						if line != "" {
							fmt.Printf("      %s\n", line)
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
	progressDone.Wait()

	return results
}

// HasFailures reports whether any result has Status "failed".
func HasFailures(results []Result) bool {
	for _, r := range results {
		if r.Status == "failed" {
			return true
		}
	}
	return false
}

// Succeeded returns the subset of results that built successfully.
func Succeeded(results []Result) []Result {
	out := make([]Result, 0)
	for _, r := range results {
		if r.Status == "success" {
			out = append(out, r)
		}
	}
	return out
}

// Failed returns the subset of results that failed to build.
func Failed(results []Result) []Result {
	out := make([]Result, 0)
	for _, r := range results {
		if r.Status == "failed" {
			out = append(out, r)
		}
	}
	return out
}
