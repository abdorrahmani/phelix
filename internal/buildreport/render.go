package buildreport

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/fatih/color"
)

// separator is the horizontal rule used inside the report block.
const separator = "  ────────────────────────────────────────────────"

// ReportView carries everything needed to print the terminal Build Report
// emitted after every successful native build.
type ReportView struct {
	AppName string
	Version int
	Tag     string
	Commit  string // full hash or ""; rendered as "unavailable"
	Report  *Report

	// Analysis, when non-nil, renders the Regression Analysis section.
	Analysis *Analysis
}

// PrintReport writes the post-build report to w. The function is total: nil
// pointers degrade to reduced output instead of panicking — reporting problems
// must never interfere with the surrounding command's exit code.
func PrintReport(w io.Writer, v ReportView) {
	r := v.Report
	if r == nil {
		return
	}
	dim := color.New(color.Faint)

	fmt.Fprintf(w, "\n%s Build Report\n", color.BlueString("→"))
	if v.AppName != "" {
		fmt.Fprintf(w, "  Application   %s\n", color.CyanString("%s", v.AppName))
	}
	versionLabel := fmt.Sprintf("v%d", v.Version)
	if v.Version <= 0 {
		versionLabel = "—"
	}
	if v.Tag != "" {
		versionLabel += fmt.Sprintf(" (%s)", v.Tag)
	}
	fmt.Fprintf(w, "  Version       %s\n", versionLabel)
	fmt.Fprintf(w, "  Compiler      %s\n", compilerDisplay(r))
	fmt.Fprintf(w, "  Duration      %s\n", FormatDurationSeconds(r.DurationMS))
	fmt.Fprintf(w, "  Cache         %s", CacheLabel(r.Cache.Status))
	if r.Cache.Source != "" {
		fmt.Fprintf(w, " %s", dim.Sprint("("+r.Cache.Source+")"))
	}
	fmt.Fprintln(w)

	switch {
	case r.Artifact.Type == ArtifactBinary && r.Artifact.SizeBytes > 0:
		fmt.Fprintf(w, "  Binary        %s", FormatBytes(r.Artifact.SizeBytes))
		if r.Artifact.Platform != "" {
			fmt.Fprintf(w, " %s", dim.Sprint("("+r.Artifact.Platform+")"))
		}
		fmt.Fprintln(w)
	case r.Artifact.Type == ArtifactDockerImage:
		fmt.Fprintf(w, "  Image         %s\n", strings.TrimSpace(r.Compiler+" image"))
	default:
		fmt.Fprintf(w, "  Binary        %s\n", dim.Sprint("unavailable"))
	}

	commit := v.Commit
	if commit == "" {
		commit = "unavailable"
	} else if len(commit) > 12 {
		commit = commit[:12]
	}
	fmt.Fprintf(w, "  Commit        %s\n", commit)

	printRegressionSection(w, v)
}

// compilerDisplay renders e.g. "Go 1.27" / "Rust 1.85".
func compilerDisplay(r *Report) string {
	name := displayName(r.Language)
	if r.CompilerVersion == "" {
		return name
	}
	return name + " " + r.CompilerVersion
}

// printRegressionSection renders the Regression Analysis block.
func printRegressionSection(w io.Writer, v ReportView) {
	a := v.Analysis
	if a == nil {
		return
	}
	dim := color.New(color.Faint)
	fmt.Fprintf(w, "%s\n", separator)
	fmt.Fprintf(w, "\n  Regression Analysis\n")

	pool := 0
	if a.Size != nil {
		pool = max(pool, a.Size.HistoryUsed)
	}
	if a.Duration != nil {
		pool = max(pool, a.Duration.HistoryUsed)
	}
	if pool > 0 {
		noun := "builds"
		if pool == 1 {
			noun = "build"
		}
		fmt.Fprintf(w, "  Compared against %s previous comparable %s.\n",
			color.GreenString("%d", pool), noun)
	}

	if a.Size != nil {
		fmt.Fprintf(w, "\n  Binary size\n")
		renderComparison(w, a.Size,
			func(n int64) string { return FormatBytes(n) },
			v.Report)
	}

	if a.Duration != nil {
		fmt.Fprintf(w, "\n  Build duration\n")
		if !a.Duration.Comparable {
			fmt.Fprintf(w, "    %s\n", color.YellowString("Comparison skipped:"))
			reason := a.Duration.SkipReason
			if reason == "" {
				reason = "comparison not possible with available metadata"
			}
			fmt.Fprintf(w, "    %s\n", dim.Sprint(reason))
		} else {
			mode := strings.ToLower(CacheLabel(v.Report.Cache.Status))
			fmt.Fprintf(w, "    Previous %s build: %s\n", mode, FormatDurationSeconds(a.Duration.Previous))
			fmt.Fprintf(w, "    Current %s build:  %s\n", mode, FormatDurationSeconds(a.Duration.Current))
			fmt.Fprintf(w, "    Change: %s (%s)\n",
				signedSeconds(a.Duration.Delta),
				pctString(a.Duration.PercentDelta))
			if a.Duration.Regression {
				fmt.Fprintf(w, "  %s Build time regression detected.\n", color.YellowString("⚠"))
			}
		}
	}
	fmt.Fprintln(w)
}

// renderComparison prints Previous/Current/Change plus the historical average
// block for a binary-size comparison.
func renderComparison(w io.Writer, mc *MetricComparison, fmtVal func(int64) string, _ *Report) {
	dim := color.New(color.Faint)
	if !mc.Comparable {
		fmt.Fprintf(w, "    %s\n", color.YellowString("Comparison skipped:"))
		reason := mc.SkipReason
		if reason == "" {
			reason = "comparison not possible with available metadata"
		}
		fmt.Fprintf(w, "    %s\n", dim.Sprint(reason))
		return
	}
	fmt.Fprintf(w, "    Previous      %s\n", fmtVal(mc.Previous))
	fmt.Fprintf(w, "    Current       %s\n", fmtVal(mc.Current))
	fmt.Fprintf(w, "    Change        %s (%s)\n",
		signedBytes(mc.Delta), pctString(mc.PercentDelta))

	if mc.HistoryUsed > 1 && mc.BaselineAvg > 0 {
		label := fmt.Sprintf("%d-build avg", mc.HistoryUsed)
		fmt.Fprintf(w, "    %-13s %s\n", label, fmtVal(mc.BaselineAvg))
		fmt.Fprintf(w, "    Change        %s (%s) vs avg\n",
			signedBytes(mc.BaselineDelta), pctString(mc.BaselinePctDelta))
	}
	if mc.Regression {
		fmt.Fprintf(w, "  %s Binary size regression detected.\n", color.YellowString("⚠"))
	}
}

// pctString renders a percentage with explicit sign; NaN renders as "—".
func pctString(p float64) string {
	if math.IsNaN(p) {
		return "—"
	}
	return fmt.Sprintf("%+.1f%%", p)
}

// signedBytes renders a byte delta with an explicit sign and human units.
func signedBytes(d int64) string {
	sign := "+"
	v := d
	if d < 0 {
		sign = "-"
		v = -d
	}
	return sign + FormatBytes(v)
}

// signedSeconds renders a millisecond delta like "+6.4s" / "-2.3s".
func signedSeconds(ms int64) string {
	return fmt.Sprintf("%+.1fs", float64(ms)/1000.0)
}

// CompactSummary is one matrix combination's post-build line.
type CompactSummary struct {
	Combination string
	Report      *Report
	Analysis    *Analysis
}

// PrintCompactSummaries renders compact per-combination build summaries after
// a matrix build. Comparisons follow the same cache-aware rules as single
// builds; incompatible combinations are filtered out inside Analyze, so each
// combination only ever references its own toolchain/platform history.
func PrintCompactSummaries(w io.Writer, summaries []CompactSummary) {
	dim := color.New(color.Faint)
	for _, s := range summaries {
		if s.Report == nil {
			continue
		}
		fmt.Fprintf(w, "  %s — Duration %s • Cache %s • Binary %s\n",
			color.CyanString("%s", s.Combination),
			FormatDurationSeconds(s.Report.DurationMS),
			CacheLabel(s.Report.Cache.Status),
			FormatBytes(s.Report.Artifact.SizeBytes))

		if s.Analysis == nil {
			continue
		}
		if s.Analysis.Size != nil && s.Analysis.Size.Comparable {
			line := fmt.Sprintf("  size vs previous comparable build: %s (%s)",
				signedBytes(s.Analysis.Size.Delta), pctString(s.Analysis.Size.PercentDelta))
			if s.Analysis.Size.Regression {
				fmt.Fprintf(w, "    %s ⚠ regression\n", line)
			} else {
				fmt.Fprintf(w, "    %s\n", dim.Sprint(line))
			}
		}
		if s.Analysis.Duration != nil && !s.Analysis.Duration.Comparable &&
			s.Analysis.Duration.SkipReason != "" {
			fmt.Fprintf(w, "    %s duration comparison skipped: %s\n",
				color.YellowString("!"), dim.Sprint(s.Analysis.Duration.SkipReason))
		} else if s.Analysis.Duration != nil && s.Analysis.Duration.Comparable {
			line := fmt.Sprintf("  duration vs previous comparable build: %s (%s)",
				signedSeconds(s.Analysis.Duration.Delta), pctString(s.Analysis.Duration.PercentDelta))
			if s.Analysis.Duration.Regression {
				fmt.Fprintf(w, "    %s ⚠ regression\n", color.YellowString(line))
			} else {
				fmt.Fprintf(w, "    %s\n", dim.Sprint(line))
			}
		}
	}
}

// FailedReportView carries data for the failed-build summary.
type FailedReportView struct {
	AppName         string
	Language        string
	CompilerVersion string
	DurationMS      int64
	Cache           CacheStatus
	Stage           string
}

// PrintFailedReport renders the concise post-mortem summary shown when a
// build fails. Failed builds are never recorded as versions, so nothing here
// persists to versions.json — this output only aids debugging.
func PrintFailedReport(w io.Writer, v FailedReportView) {
	stage := v.Stage
	if stage == "" {
		stage = "unknown stage"
	}
	fmt.Fprintf(w, "  Failed after %s (stage: %s)\n",
		color.YellowString(FormatDurationSeconds(v.DurationMS)), stage)
	name := displayName(v.Language)
	compiler := name
	if v.CompilerVersion != "" {
		compiler = name + " " + v.CompilerVersion
	}
	parts := []string{"Compiler: " + compiler, "Cache: " + CacheLabel(v.Cache)}
	fmt.Fprintf(w, "    %s\n", color.New(color.Faint).Sprint(strings.Join(parts, " • ")))
}

func displayName(lang string) string {
	switch lang {
	case "go":
		return "Go"
	case "rust":
		return "Rust"
	default:
		if lang == "" {
			return "unknown"
		}
		return lang
	}
}

// StoredReportView is one stored version entry rendered by the read-only
// build-report inspection command.
type StoredReportView struct {
	Version int
	Tag     string
	Commit  string
	BuiltAt time.Time
	Report  *Report
}

// PrintStoredReports lists recent stored build reports newest-first. It never
// triggers builds and works entirely offline against versions.json.
func PrintStoredReports(w io.Writer, appName string, views []StoredReportView) {
	if len(views) == 0 {
		fmt.Fprintf(w, "  No build reports recorded for %s yet.\n", color.CyanString("'%s'", appName))
		return
	}
	dim := color.New(color.Faint)
	for _, sv := range views {
		r := sv.Report
		fmt.Fprintf(w, "%s v%d%s  %s\n",
			color.BlueString("→"),
			sv.Version,
			tagSuffix(sv.Tag),
			dim.Sprint(sv.BuiltAt.Format(time.RFC3339)))
		if r == nil {
			fmt.Fprintf(w, "    %s\n", dim.Sprint(
				"no build report metadata (pre-dates build reporting)"))
			continue
		}
		size := "unavailable"
		if r.Artifact.SizeBytes > 0 {
			size = FormatBytes(r.Artifact.SizeBytes)
			if r.Artifact.Platform != "" {
				size += " (" + r.Artifact.Platform + ")"
			}
		}
		commit := sv.Commit
		if commit == "" {
			commit = "unavailable"
		} else if len(commit) > 12 {
			commit = commit[:12]
		}
		args := "—"
		if len(r.BuildArgs) > 0 {
			args = strings.Join(r.BuildArgs, " ")
		}
		fmt.Fprintf(w, "    Compiler   %s\n", compilerDisplay(r))
		fmt.Fprintf(w, "    Duration   %s\n", FormatDurationSeconds(r.DurationMS))
		cacheLine := CacheLabel(r.Cache.Status)
		if r.Cache.Source != "" {
			cacheLine += " (" + r.Cache.Source + ")"
		}
		fmt.Fprintf(w, "    Cache      %s\n", cacheLine)
		fmt.Fprintf(w, "    Binary     %s\n", size)
		fmt.Fprintf(w, "    Commit     %s\n", commit)
		fmt.Fprintf(w, "    Args       %s\n", args)
	}
}

func tagSuffix(tag string) string {
	if tag == "" {
		return ""
	}
	return " (" + tag + ")"
}
