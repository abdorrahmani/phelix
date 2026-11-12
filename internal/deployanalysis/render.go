package deployanalysis

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/table"
	"github.com/fatih/color"
)

// PrintReport writes one deployment analysis to w. Nil metrics are rendered as
// "unavailable", never as zero.
func PrintReport(w io.Writer, rep *Report, detailed bool) {
	if rep == nil {
		fmt.Fprintln(w, "No deployment analysis available.")
		return
	}
	dim := color.New(color.Faint)
	fmt.Fprintf(w, "\n%s Deployment Analysis\n", color.BlueString("→"))
	fmt.Fprintf(w, "  Application   %s\n", color.CyanString("%s", rep.AppName))
	fmt.Fprintf(w, "  Deployment    %s\n", displayOr(rep.DeploymentID, "unknown"))
	fmt.Fprintf(w, "  Strategy      %s\n", displayOr(rep.Strategy, "unknown"))
	fmt.Fprintf(w, "  Versions      %s\n", versionTransition(rep.PreviousVersion, rep.NewVersion))
	fmt.Fprintf(w, "  Outcome       %s\n", displayOr(rep.Outcome, "unknown"))
	fmt.Fprintf(w, "  Status        %s\n", statusLabel(rep.Status))
	if rep.ImpactScore < 0 {
		fmt.Fprintf(w, "  Impact score  unavailable\n")
	} else {
		fmt.Fprintf(w, "  Impact score  %s (grade %s)\n", color.New(color.Bold).Sprintf("%.1f/100", rep.ImpactScore), rep.ImpactGrade)
	}
	fmt.Fprintf(w, "  Recommendation %s\n", rep.Recommendation)
	if rep.Rollout != nil {
		printRollout(w, rep.Rollout)
	}

	if detailed {
		printMetrics(w, rep)
	} else {
		fmt.Fprintf(w, "\n  %s\n", dim.Sprint("Use --detailed to see every metric comparison."))
	}
	fmt.Fprintf(w, "  Analyzed      %s\n", rep.AnalyzedAt.Local().Format("2006-01-02 15:04:05"))
}

func printRollout(w io.Writer, r *Rollout) {
	fmt.Fprintf(w, "  Rollout       %s, %s\n", displayOr(r.Strategy, "unknown"), rolloutPhase(r))
	if r.FailedStep > 0 {
		fmt.Fprintf(w, "  Failed step   %d of %d\n", r.FailedStep, r.TotalSteps)
	}
	if r.Reason != "" {
		fmt.Fprintf(w, "  Rollout note  %s\n", r.Reason)
	}
	if r.Verification != nil && r.Verification.SkipReason != "" {
		fmt.Fprintf(w, "  Verification  %s\n", r.Verification.SkipReason)
	}
}

func rolloutPhase(r *Rollout) string {
	switch {
	case r.Promoted:
		return "promoted"
	case r.FailureCode != "":
		return "rejected"
	default:
		return "not promoted"
	}
}

func printMetrics(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "\n  Metric comparisons\n")
	t := table.New(w, false)
	t.Header([]string{"Metric", "Before", "After", "Change", "Status"})
	for i := range rep.Metrics {
		m := &rep.Metrics[i]
		t.Append([]string{
			m.Label,
			metricValue(m.Key, m.Before),
			metricValue(m.Key, m.After),
			metricChange(m),
			statusLabel(m.Status),
		})
	}
	_ = t.Render()

	var notes []string
	for i := range rep.Metrics {
		m := &rep.Metrics[i]
		if m.SkipReason != "" {
			notes = append(notes, fmt.Sprintf("%s: %s", m.Label, m.SkipReason))
		}
	}
	if len(notes) > 0 {
		fmt.Fprintf(w, "\n  Notes\n")
		for _, note := range notes {
			fmt.Fprintf(w, "  - %s\n", note)
		}
	}

	if hasPeak(rep, MetricCPUUsage) {
		fmt.Fprintf(w, "\n  Peak CPU      %s before, %s after\n",
			metricValue(MetricCPUUsage, rep.Metric(MetricCPUUsage).BeforePeak),
			metricValue(MetricCPUUsage, rep.Metric(MetricCPUUsage).AfterPeak))
	}
	if hasPeak(rep, MetricMemoryUsage) {
		fmt.Fprintf(w, "  Peak memory   %s before, %s after\n",
			metricValue(MetricMemoryUsage, rep.Metric(MetricMemoryUsage).BeforePeak),
			metricValue(MetricMemoryUsage, rep.Metric(MetricMemoryUsage).AfterPeak))
	}
	fmt.Fprintf(w, "\n  Windows\n")
	fmt.Fprintf(w, "  Baseline      %s to %s (%d samples)\n",
		formatTime(rep.Baseline.StartedAt), formatTime(rep.Baseline.EndedAt), rep.Baseline.Samples)
	fmt.Fprintf(w, "  Observed      %s to %s (%d samples)\n",
		formatTime(rep.Observed.StartedAt), formatTime(rep.Observed.EndedAt), rep.Observed.Samples)
}

func hasPeak(rep *Report, key string) bool {
	m := rep.Metric(key)
	return m != nil && m.BeforePeak != nil && m.AfterPeak != nil
}

// PrintList writes the recent reports as a compact history table.
func PrintList(w io.Writer, appName string, reports []Report) {
	if len(reports) == 0 {
		fmt.Fprintf(w, "No deployment analyses recorded for %s.\n", appName)
		return
	}
	t := table.New(w, false)
	t.Header([]string{"Deployed", "Versions", "Strategy", "Outcome", "Score", "Status"})
	for i := range reports {
		r := &reports[i]
		t.Append([]string{
			formatTime(r.DeployedAt),
			versionTransition(r.PreviousVersion, r.NewVersion),
			displayOr(r.Strategy, "unknown"),
			displayOr(r.Outcome, "unknown"),
			scoreText(r.ImpactScore),
			r.Status,
		})
	}
	_ = t.Render()
	fmt.Fprintf(w, "\nUse `phelix deployment-report %s --detailed` for the latest report.\n", appName)
}

func metricValue(key string, v *float64) string {
	if v == nil {
		return "unavailable"
	}
	switch key {
	case MetricMemoryUsage:
		return formatBytes(*v)
	case MetricP95Latency:
		return fmt.Sprintf("%.1f ms", *v)
	case MetricThroughput:
		return fmt.Sprintf("%.2f req/s", *v)
	case MetricErrorRate, MetricCPUUsage:
		return fmt.Sprintf("%.2f%%", *v)
	case MetricRestartCount:
		return fmt.Sprintf("%.0f", *v)
	case MetricHealthFailures:
		return fmt.Sprintf("%.0f", *v)
	default:
		return fmt.Sprintf("%.2f", *v)
	}
}

func metricChange(m *MetricComparison) string {
	if m.Status == StatusNotAvailable || m.Status == StatusInsufficientData {
		return "-"
	}
	if m.Delta == nil {
		return "-"
	}
	if m.Key == MetricRestartCount || m.Key == MetricHealthFailures {
		return fmt.Sprintf("%+.0f", *m.Delta)
	}
	if m.Percent == nil {
		return metricValue(m.Key, m.Delta)
	}
	sign := ""
	if *m.Percent > 0 {
		sign = "+"
	}
	return fmt.Sprintf("%s%.1f%%", sign, *m.Percent)
}

func statusLabel(status string) string {
	switch status {
	case StatusRegression:
		return color.RedString("REGRESSION")
	case StatusImprovement:
		return color.GreenString("IMPROVEMENT")
	case StatusInsufficientData:
		return color.YellowString("INSUFFICIENT DATA")
	case StatusNotAvailable:
		return color.New(color.Faint).Sprint("NOT AVAILABLE")
	default:
		return color.GreenString("NORMAL")
	}
}

func scoreText(score float64) string {
	if score < 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", score)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func formatBytes(v float64) string {
	const (
		kib = 1024
		mib = kib * 1024
		gib = mib * 1024
	)
	switch {
	case v >= gib:
		return fmt.Sprintf("%.2f GiB", v/gib)
	case v >= mib:
		return fmt.Sprintf("%.1f MiB", v/mib)
	case v >= kib:
		return fmt.Sprintf("%.1f KiB", v/kib)
	default:
		return fmt.Sprintf("%.0f B", v)
	}
}

func versionTransition(prev, next string) string {
	prev = displayOr(prev, "unknown")
	next = displayOr(next, "unknown")
	return strings.TrimSpace(prev + " to " + next)
}

func displayOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
