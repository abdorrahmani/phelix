package buildreport

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintReport_IncludesCoreFields(t *testing.T) {
	var buf bytes.Buffer
	rep := repWith("go", "1.27", "linux/amd64", 15518924, 31200, CacheCold)
	PrintReport(&buf, ReportView{
		AppName:  "api",
		Version:  12,
		Commit:   "8f31c2a1234567890",
		Report:   rep,
		Analysis: Analyze(rep, nil, DefaultConfig()),
	})
	out := buf.String()
	for _, want := range []string{
		"Build Report", "api", "v12", "Go 1.27", "31.2s", "COLD", "14.8 MB", "8f31c2a", "linux/amd64",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report output missing %q; got:\n%s", want, out)
		}
	}
	// Regression section renders even without analysis input.
	if !strings.Contains(out, "Regression Analysis") {
		t.Errorf("expected regression section header")
	}
}

func TestPrintReport_CommitUnavailable(t *testing.T) {
	var buf bytes.Buffer
	rep := repWith("go", "1.27", "linux/amd64", mb, 1000, CacheHit)
	PrintReport(&buf, ReportView{Version: 1, Report: rep})
	if !strings.Contains(buf.String(), "unavailable") {
		t.Errorf("non-git projects must show commit as unavailable; got:\n%s", buf.String())
	}
}

func TestPrintReport_SkippedDurationNoFalsePercentage(t *testing.T) {
	var buf bytes.Buffer
	coldPrev := repWith("go", "1.27", "linux/amd64", 14*mb, 21400, CacheCold)
	hitCur := repWith("go", "1.27", "linux/amd64", 14*mb+500_000, 5200, CacheHit)
	a := Analyze(hitCur, hist(coldPrev), DefaultConfig())
	PrintReport(&buf, ReportView{Version: 11, Report: hitCur, Analysis: a})
	out := buf.String()

	if !strings.Contains(out, "Comparison skipped:") {
		t.Errorf("cache-mode mismatch must render an explicit skip; got:\n%s", out)
	}
	if !strings.Contains(out, "cache mode differs") {
		t.Errorf("skip must explain why; got:\n%s", out)
	}
	// The forbidden false claim must never appear for a 21.4s→5.2s jump:
	if strings.Contains(out, "faster") {
		t.Errorf("no misleading speedup claim allowed:\n%s", out)
	}
	// Binary size section is still populated (cache-independent).
	if !strings.Contains(out, "Previous      14.0 MB") {
		t.Errorf("binary size comparison must remain visible:\n%s", out)
	}
}

func TestPrintReport_RegressionAlerts(t *testing.T) {
	var buf bytes.Buffer
	prev := repWith("go", "1.27", "linux/amd64", 20*mb, 24000, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", 20*mb+2*mb, 32000, CacheCold) // +10% size, +33% time
	a := Analyze(cur, hist(prev), DefaultConfig())
	if !a.Size.Regression || !a.Duration.Regression {
		t.Fatalf("test setup: both metrics must regress (size=%v dur=%v)", a.Size.Regression, a.Duration.Regression)
	}
	PrintReport(&buf, ReportView{Version: 5, Report: cur, Analysis: a})
	out := buf.String()
	if !strings.Contains(out, "Binary size regression detected") {
		t.Errorf("missing binary regression alert:\n%s", out)
	}
	if !strings.Contains(out, "Build time regression detected") {
		t.Errorf("missing time regression alert:\n%s", out)
	}
	if !strings.Contains(out, "5-build avg") && !strings.Contains(out, "1-build avg") {
		// single-entry history: baseline may or may not render, but Change lines must
		if !strings.Contains(out, "Change") {
			t.Errorf("expected change line; got:\n%s", out)
		}
	}
}

func TestPrintReport_NoAlertBelowThresholds(t *testing.T) {
	var buf bytes.Buffer
	prev := repWith("go", "1.27", "linux/amd64", 14*mb+100_000, 24800, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", prev.Artifact.SizeBytes+700_000, 25500, CacheCold)
	a := Analyze(cur, hist(prev), DefaultConfig())
	PrintReport(&buf, ReportView{Version: 4, Report: cur, Analysis: a})
	if strings.Contains(buf.String(), "regression detected") {
		t.Errorf("sub-threshold changes must not alert:\n%s", buf.String())
	}
}

func TestPrintFailedReport(t *testing.T) {
	var buf bytes.Buffer
	PrintFailedReport(&buf, FailedReportView{
		Language: "rust", CompilerVersion: "1.85", DurationMS: 12300,
		Cache: CacheCold, Stage: "compile",
	})
	out := buf.String()
	for _, want := range []string{"12.3s", "compile", "Rust 1.85", "COLD"} {
		if !strings.Contains(out, want) {
			t.Errorf("failed report missing %q; got:\n%s", want, out)
		}
	}
}

func TestPrintStoredReports(t *testing.T) {
	var buf bytes.Buffer
	PrintStoredReports(&buf, "api", nil)
	if !strings.Contains(buf.String(), "No build reports") {
		t.Errorf("empty history must render friendly notice; got:\n%s", buf.String())
	}

	buf.Reset()
	views := []StoredReportView{
		{Version: 2, Report: repWith("go", "1.27", "linux/amd64", 15*mb, 5200, CacheHit)},
		{Version: 1, Commit: "abc", Report: nil}, // legacy version
	}
	PrintStoredReports(&buf, "api", views)
	out := buf.String()
	if !strings.Contains(out, "v1") || !strings.Contains(out, "no build report metadata") {
		t.Errorf("legacy versions must render gracefully:\n%s", out)
	}
	if !strings.Contains(out, "15.0 MB") { // 15*mb renders as 15.0 MB → "15.0 MB"; repWith sets 15*mb via repWith? no: 15*mb
		t.Errorf("unexpected size rendering:\n%s", out)
	}
}
