package deployanalysis

import (
	"fmt"
	"math"
	"time"
)

// metricDefinition describes one metric's presentation and direction. Higher
// is worse for all of them except throughput, where lower is worse.
type metricDefinition struct {
	Key         string
	Label       string
	Unit        string
	Weight      float64
	LowerBetter bool
}

var metricDefinitions = []metricDefinition{
	{Key: MetricErrorRate, Label: "Error rate", Unit: "%", Weight: 3},
	{Key: MetricP95Latency, Label: "P95 latency", Unit: "ms", Weight: 3},
	{Key: MetricHealthFailures, Label: "Health failures", Unit: "checks", Weight: 3},
	{Key: MetricRestartCount, Label: "Restarts", Unit: "restarts", Weight: 2},
	{Key: MetricThroughput, Label: "Throughput", Unit: "req/s", Weight: 2, LowerBetter: true},
	{Key: MetricCPUUsage, Label: "CPU average", Unit: "%", Weight: 1},
	{Key: MetricMemoryUsage, Label: "Memory average", Unit: "bytes", Weight: 1},
}

func floatPtr(v float64) *float64 {
	out := roundTo(v, 4)
	return &out
}

func percentDelta(before, after float64) *float64 {
	if before == 0 {
		return nil
	}
	return floatPtr((after - before) / before * 100)
}

// compareMetrics builds the full metric comparison list from two window
// aggregates. A metric whose data is missing is marked NOT_AVAILABLE, and one
// whose window has too few samples is marked INSUFFICIENT_DATA. Neither is
// ever reported as a normal zero.
func compareMetrics(before, after WindowAggregate, cfg Config) []MetricComparison {
	out := make([]MetricComparison, 0, len(metricDefinitions))
	for _, def := range metricDefinitions {
		mc := MetricComparison{
			Key:             def.Key,
			Label:           def.Label,
			Unit:            def.Unit,
			BaselineSamples: before.ResourceSamples,
			ObservedSamples: after.ResourceSamples,
		}
		switch def.Key {
		case MetricErrorRate:
			mc.BaselineSamples, mc.ObservedSamples = before.TrafficSamples, after.TrafficSamples
			setValues(&mc, before.HasTraffic && before.Requests > 0, before.ErrorRate,
				after.HasTraffic && after.Requests > 0, after.ErrorRate)
			evaluate(&mc, def, before.ErrorRate, after.ErrorRate, cfg.ErrorRateMaxDelta, false)
		case MetricP95Latency:
			mc.BaselineSamples, mc.ObservedSamples = before.TrafficSamples, after.TrafficSamples
			setValues(&mc, before.HasTraffic, before.P95MS, after.HasTraffic, after.P95MS)
			evaluate(&mc, def, before.P95MS, after.P95MS, cfg.LatencyMaxPercent, false)
		case MetricCPUUsage:
			setValues(&mc, before.HasCPU, before.CPUAvg, after.HasCPU, after.CPUAvg)
			if before.HasCPU {
				mc.BeforePeak = floatPtr(before.CPUPeak)
			}
			if after.HasCPU {
				mc.AfterPeak = floatPtr(after.CPUPeak)
			}
			evaluate(&mc, def, before.CPUAvg, after.CPUAvg, cfg.CPUMaxPercent, false)
		case MetricMemoryUsage:
			setValues(&mc, before.HasMemory, before.MemoryAvg, after.HasMemory, after.MemoryAvg)
			if before.HasMemory {
				mc.BeforePeak = floatPtr(before.MemoryPeak)
			}
			if after.HasMemory {
				mc.AfterPeak = floatPtr(after.MemoryPeak)
			}
			evaluate(&mc, def, before.MemoryAvg, after.MemoryAvg, cfg.MemoryMaxPercent, false)
		case MetricThroughput:
			mc.BaselineSamples, mc.ObservedSamples = before.TrafficSamples, after.TrafficSamples
			setValues(&mc, before.HasTraffic && before.Throughput > 0, before.Throughput,
				after.HasTraffic && after.Throughput > 0, after.Throughput)
			evaluate(&mc, def, before.Throughput, after.Throughput, cfg.ThroughputMaxDegradation, true)
		case MetricRestartCount:
			mc.BaselineSamples, mc.ObservedSamples = before.HealthChecks, after.HealthChecks
			mc.Before, mc.After = floatPtr(float64(before.Restarts)), floatPtr(float64(after.Restarts))
			mc.Delta = floatPtr(float64(after.Restarts - before.Restarts))
			countStatus(&mc, before.HealthChecks, after.HealthChecks, after.Restarts-before.Restarts, cfg.RestartMaxExtra)
		case MetricHealthFailures:
			mc.BaselineSamples, mc.ObservedSamples = before.HealthChecks, after.HealthChecks
			mc.Before, mc.After = floatPtr(float64(before.HealthFailures)), floatPtr(float64(after.HealthFailures))
			mc.Delta = floatPtr(float64(after.HealthFailures - before.HealthFailures))
			countStatus(&mc, before.HealthChecks, after.HealthChecks, after.HealthFailures-before.HealthFailures, cfg.HealthFailuresMaxExtra)
		}
		out = append(out, mc)
	}
	return out
}

func setValues(mc *MetricComparison, beforeOK bool, beforeVal float64, afterOK bool, afterVal float64) {
	if beforeOK {
		mc.Before = floatPtr(beforeVal)
	}
	if afterOK {
		mc.After = floatPtr(afterVal)
	}
	if mc.Before != nil && mc.After != nil {
		mc.Delta = floatPtr(*mc.After - *mc.Before)
		mc.Percent = percentDelta(*mc.Before, *mc.After)
	}
}

// evaluate assigns the status for a numeric metric. threshold is absolute for
// error rate (percentage points) and a percentage change for the rest.
func evaluate(mc *MetricComparison, def metricDefinition, before, after, threshold float64, lowerBetter bool) {
	if mc.Before == nil && mc.After == nil {
		mc.Status = StatusNotAvailable
		mc.SkipReason = "the metric was not recorded in either window"
		return
	}
	if mc.Before == nil || mc.After == nil {
		mc.Status = StatusInsufficientData
		mc.SkipReason = "the metric was recorded in only one of the windows"
		return
	}
	if mc.BaselineSamples < 1 || mc.ObservedSamples < 1 {
		mc.Status = StatusInsufficientData
		mc.SkipReason = "no samples in one of the windows"
		return
	}
	if mc.BaselineSamples < minSamplesFloor || mc.ObservedSamples < minSamplesFloor {
		mc.Status = StatusInsufficientData
		mc.SkipReason = fmt.Sprintf("fewer than %d samples in one of the windows", minSamplesFloor)
		return
	}

	var regression, improvement bool
	if def.Key == MetricErrorRate {
		delta := after - before
		regression = delta > threshold
		improvement = delta < -threshold
	} else {
		if mc.Percent == nil {
			if before == 0 && after == 0 {
				mc.Status = StatusNormal
				return
			}
			mc.Status = StatusInsufficientData
			mc.SkipReason = "the baseline value was zero, so a percentage change cannot be computed"
			return
		}
		change := *mc.Percent
		if lowerBetter {
			regression = change < -threshold
			improvement = change > threshold
		} else {
			regression = change > threshold
			improvement = change < -threshold
		}
	}

	switch {
	case regression:
		mc.Status = StatusRegression
	case improvement:
		mc.Status = StatusImprovement
	default:
		mc.Status = StatusNormal
	}
}

// minSamplesFloor is the absolute minimum even when a project asks for fewer:
// a single sample is never a window.
const minSamplesFloor = 2

func countStatus(mc *MetricComparison, beforeChecks, afterChecks, extra, threshold int) {
	if beforeChecks == 0 && afterChecks == 0 {
		mc.Status = StatusNotAvailable
		mc.SkipReason = "the health daemon recorded no checks"
		return
	}
	if afterChecks < minSamplesFloor || beforeChecks < minSamplesFloor {
		mc.Status = StatusInsufficientData
		mc.SkipReason = "too few health checks in one of the windows"
		return
	}
	switch {
	case extra > threshold:
		mc.Status = StatusRegression
	case extra < -threshold:
		mc.Status = StatusImprovement
	default:
		mc.Status = StatusNormal
	}
}

// applyMinSamples overrides the sample counts of metrics evaluated before the
// configured minimum was known. compareMetrics uses the absolute floor; this
// re-evaluates metrics that fall between the floor and the configured minimum.
func applyMinSamples(metrics []MetricComparison, cfg Config) {
	min := cfg.MinSamples
	if min < minSamplesFloor {
		min = minSamplesFloor
	}
	for i := range metrics {
		m := &metrics[i]
		if m.Status == StatusInsufficientData || m.Status == StatusNormal ||
			m.Status == StatusRegression || m.Status == StatusImprovement {
			if (m.BaselineSamples > 0 && m.BaselineSamples < min) ||
				(m.ObservedSamples > 0 && m.ObservedSamples < min) {
				m.Status = StatusInsufficientData
				m.SkipReason = fmt.Sprintf("fewer than %d samples in one of the windows", min)
			}
		}
	}
}

// scoreReport computes the transparent Deployment Impact Score. The score is
// 100 when every measured metric stayed inside its threshold, and falls toward
// 0 as weighted regressions grow. Improvements can add back at most 10 points.
// Unavailable metrics never contribute, so missing data cannot inflate or
// deflate the score.
func scoreReport(metrics []MetricComparison, cfg Config) (score float64, grade string) {
	var availableWeight, penaltyWeight, creditWeight float64
	for _, mc := range metrics {
		def, ok := definition(mc.Key)
		if !ok || mc.Status == StatusNotAvailable || mc.Status == StatusInsufficientData {
			continue
		}
		availableWeight += def.Weight
		severity := severityOf(mc, cfg)
		switch mc.Status {
		case StatusRegression:
			penaltyWeight += def.Weight * severity
		case StatusImprovement:
			creditWeight += def.Weight * severity
		}
	}
	if availableWeight == 0 {
		return -1, "N/A"
	}
	score = 100
	score -= 60 * penaltyWeight / availableWeight
	score += 10 * creditWeight / availableWeight
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	score = roundTo(score, 1)
	return score, gradeFor(score)
}

func gradeFor(score float64) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 75:
		return "B"
	case score >= 60:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "F"
	}
}

func definition(key string) (metricDefinition, bool) {
	for _, def := range metricDefinitions {
		if def.Key == key {
			return def, true
		}
	}
	return metricDefinition{}, false
}

// severityOf maps a metric's deviation to a 0..1 severity, where 1 means three
// times the configured threshold (or a step change from zero).
func severityOf(mc MetricComparison, cfg Config) float64 {
	if mc.Delta == nil {
		return 0
	}
	threshold := 0.0
	switch mc.Key {
	case MetricErrorRate:
		threshold = cfg.ErrorRateMaxDelta
		if *mc.Before == 0 && *mc.After > 0 {
			return 1
		}
		if threshold <= 0 {
			return 0
		}
		return clamp01(math.Abs(*mc.Delta) / (threshold * 3))
	case MetricP95Latency:
		threshold = cfg.LatencyMaxPercent
	case MetricCPUUsage:
		threshold = cfg.CPUMaxPercent
	case MetricMemoryUsage:
		threshold = cfg.MemoryMaxPercent
	case MetricThroughput:
		threshold = cfg.ThroughputMaxDegradation
	case MetricRestartCount:
		return clamp01(math.Abs(*mc.Delta) / 3)
	case MetricHealthFailures:
		return clamp01(math.Abs(*mc.Delta) / 3)
	}
	if threshold <= 0 || mc.Percent == nil {
		return 0
	}
	return clamp01(math.Abs(*mc.Percent) / (threshold * 3))
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// overallStatus reduces the metric list to the report status.
func overallStatus(metrics []MetricComparison) string {
	available, insufficient, regressions, improvements := 0, 0, 0, 0
	for _, mc := range metrics {
		switch mc.Status {
		case StatusRegression:
			available++
			regressions++
		case StatusImprovement:
			available++
			improvements++
		case StatusNormal:
			available++
		case StatusInsufficientData:
			insufficient++
		}
	}
	switch {
	case available == 0 && insufficient == 0:
		return StatusNotAvailable
	case available == 0:
		return StatusInsufficientData
	case regressions > 0:
		return StatusRegression
	case improvements > 0:
		return StatusImprovement
	default:
		return StatusNormal
	}
}

// recommendation renders the status as one actionable sentence.
func recommendation(status string, metrics []MetricComparison) string {
	switch status {
	case StatusRegression:
		names := make([]string, 0, 2)
		for _, mc := range metrics {
			if mc.Status == StatusRegression && len(names) < 2 {
				names = append(names, mc.Label)
			}
		}
		if len(names) > 0 {
			return fmt.Sprintf("Deployment promoted with regressions in %s. Review the affected metrics and consider a rollback if the impact persists.", joinNames(names))
		}
		return "Deployment showed regressions. Review the affected metrics and consider a rollback if the impact persists."
	case StatusImprovement:
		return "Deployment improved measured performance. No action required."
	case StatusNormal:
		return "Deployment is within the configured thresholds. No action required."
	case StatusInsufficientData:
		return "Not enough monitoring data to assess this deployment. Analysis will be incomplete until the observation window has more samples."
	default:
		return "No monitoring data is available for this deployment, so its impact cannot be assessed."
	}
}

func joinNames(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return names[0] + " and " + names[1]
}

// Analyze builds a complete report from two aggregated windows and the
// deployment metadata. It never fabricates a value: metrics with no samples
// remain unavailable.
func Analyze(input ReportInput, baseline, observed WindowAggregate, cfg Config) *Report {
	metrics := compareMetrics(baseline, observed, cfg)
	applyMinSamples(metrics, cfg)
	status := overallStatus(metrics)
	score, grade := scoreReport(metrics, cfg)

	rep := &Report{
		SchemaVersion:   SchemaVersion,
		AnalysisID:      input.AnalysisID,
		DeploymentID:    input.DeploymentID,
		AppName:         input.AppName,
		AppID:           input.AppID,
		ServerID:        input.ServerID,
		Strategy:        input.Strategy,
		PreviousVersion: input.PreviousVersion,
		NewVersion:      input.NewVersion,
		Outcome:         outcomeFor(input, status),
		Status:          status,
		Recommendation:  recommendation(status, metrics),
		ImpactScore:     score,
		ImpactGrade:     grade,
		Baseline:        windowOf(baseline),
		Observed:        windowOf(observed),
		Metrics:         metrics,
		Rollout:         input.Rollout,
		FailureCode:     input.FailureCode,
		DeployedAt:      input.DeployedAt,
		CompletedAt:     input.CompletedAt,
		AnalyzedAt:      time.Now().UTC(),
	}
	return rep
}

func windowOf(agg WindowAggregate) Window {
	return Window{
		StartedAt: agg.StartedAt,
		EndedAt:   agg.EndedAt,
		Samples:   max(agg.ResourceSamples, agg.TrafficSamples, agg.HealthChecks),
	}
}

func outcomeFor(input ReportInput, status string) string {
	if input.Outcome != "" && input.Outcome != OutcomePromoted {
		return input.Outcome
	}
	switch status {
	case StatusNotAvailable, StatusInsufficientData:
		return OutcomeInsufficientData
	case StatusRegression:
		return OutcomePromotedWithRegressions
	default:
		return OutcomePromoted
	}
}
