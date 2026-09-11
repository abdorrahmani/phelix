package matrix

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// ReportFromRun is the run-level report source: a resumed run's report.json
// must describe every combination of the run (earlier sessions included), not
// just the last session's results.

func reportTestRun() *Run {
	run := NewRun("mx_20260910_aa11", "app", "/tmp/p", &Profile{
		Lang: builder.Go, Versions: []string{"1.26", "1.27"},
		Platforms: []string{"linux/amd64", "linux/arm64"}, Concurrency: 2, Retries: 1,
	}, time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC))
	run.InitCombinations([]Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	})
	return run
}

func TestReportFromRun_DescribesWholeRun(t *testing.T) {
	run := reportTestRun()

	// A combination that succeeded in an earlier (interrupted) session.
	run.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success", Artifact: "/tmp/earlier-bin", SHA256: strings.Repeat("a", SHA256HexLen),
	})
	// A combination that failed in the current session after a retry.
	run.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
		Status:      "failed", Error: errors.New("build failed: token=sekrit"),
		Attempts: []Attempt{
			{Number: 1, Status: "failed", Duration: time.Second, Error: errors.New("boom")},
			{Number: 2, Status: "failed", Duration: 2 * time.Second, Error: errors.New("boom")},
		},
	})
	// Two combinations never ran (the run is interrupted).
	run.Finalize(time.Date(2026, 9, 10, 10, 2, 0, 0, time.UTC))
	if run.Status != RunStatusInterrupted {
		t.Fatalf("fixture status = %s, want interrupted", run.Status)
	}

	rep := ReportFromRun(run)
	if rep.RunID != "mx_20260910_aa11" || rep.AppName != "app" || rep.Lang != "go" {
		t.Fatalf("report header: %+v", rep)
	}
	if rep.Total != 4 || rep.Succeeded != 1 || rep.Failed != 1 || rep.Pending != 2 {
		t.Fatalf("counters: total=%d succ=%d fail=%d pending=%d, want 4/1/1/2",
			rep.Total, rep.Succeeded, rep.Failed, rep.Pending)
	}
	if rep.Duration != run.Duration {
		t.Fatalf("duration = %q, want run duration %q", rep.Duration, run.Duration)
	}

	byStatus := map[string]ComboReport{}
	for _, cr := range rep.Combinations {
		byStatus[cr.Status] = cr
	}
	earlier, ok := byStatus["success"]
	if !ok || earlier.Artifact != "/tmp/earlier-bin" || earlier.SHA256 != strings.Repeat("a", SHA256HexLen) {
		t.Fatalf("earlier-session success missing from run-level report: %+v", earlier)
	}
	failed, ok := byStatus["failed"]
	if !ok || len(failed.AttemptLog) != 2 || failed.AttemptLog[1].Number != 2 {
		t.Fatalf("failed combination attempt history: %+v", failed)
	}
	if _, ok := byStatus["pending"]; !ok {
		t.Fatalf("incomplete combinations must appear as pending, got %+v", rep.Combinations)
	}
}

func TestReportFromRun_CounterInvariant(t *testing.T) {
	run := reportTestRun()
	run.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success",
	})
	run.MarkRunning("go1.26-linux-arm64", 1, time.Now())
	run.Finalize(time.Now())

	rep := ReportFromRun(run)
	if rep.Succeeded+rep.Failed+rep.Skipped+rep.Pending != rep.Total {
		t.Fatalf("counters inconsistent: %+v", rep)
	}
	if rep.Pending != 3 || rep.Succeeded != 1 {
		t.Fatalf("counters: %+v", rep)
	}
}

func TestReportFromRun_NilRun(t *testing.T) {
	rep := ReportFromRun(nil)
	if rep.Total != 0 || len(rep.Combinations) != 0 {
		t.Fatalf("nil run must yield an empty report, got %+v", rep)
	}
}

func TestReportFromRun_RedactsPersistedErrors(t *testing.T) {
	// Errors are redacted when recorded into the run; ReportFromRun passes the
	// stored (already redacted) text through without re-rendering secrets.
	run := reportTestRun()
	run.RecordResult(Result{
		Combination: Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "failed", Error: errors.New("build failed with password=hunter2"),
	})
	run.Finalize(time.Now())

	rep := ReportFromRun(run)
	if rep.Combinations[0].Error == "" || strings.Contains(rep.Combinations[0].Error, "hunter2") {
		t.Fatalf("error not redacted: %q", rep.Combinations[0].Error)
	}
}
