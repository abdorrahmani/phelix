package buildreport

import (
	"fmt"
	"math"
	"time"
)

// HistoryEntry is one previous build offered to Analyze. Report may be nil for
// versions created before build reports existed (or whose metadata was
// unreadable); such versions are simply unavailable for regression comparison
// and never break analysis.
type HistoryEntry struct {
	Version int
	Tag     string
	BuiltAt time.Time
	Report  *Report
}

// MetricComparison is the result of evaluating one metric against history.
// When Comparable is false, SkipReason explains why (and no percentages must
// be rendered).
type MetricComparison struct {
	Comparable bool
	SkipReason string

	Previous int64 // previous comparable value (bytes or ms)
	Current  int64 // current value (bytes or ms)
	Delta    int64 // current - previous (signed)

	// PercentDelta is Delta/Previous*100 when Previous > 0, math.NaN() when
	// undefined (renders as "—").
	PercentDelta float64

	// Regression indicates the increase crossed the configured thresholds.
	Regression bool

	// Baseline describes the historical average over the previous comparable
	// builds inside the window (HistoryUsed builds). HistoryUsed == 0 means
	// no baseline exists yet.
	HistoryUsed      int
	BaselineAvg      int64
	BaselineDelta    int64
	BaselinePctDelta float64
}

// Analysis is the full regression evaluation of one build report.
type Analysis struct {
	Size     *MetricComparison // nil-safe; never a misleading percentage
	Duration *MetricComparison

	// SkippedNoMetadata counts history versions lacking usable build-report
	// metadata (excluded from every comparison).
	SkippedNoMetadata int

	// HistoryTotal is the number of history entries supplied before filtering.
	HistoryTotal int
}

// AnyRegressed reports whether any evaluated metric crossed a threshold.
func (a *Analysis) AnyRegressed() bool {
	if a == nil {
		return false
	}
	return (a.Size != nil && a.Size.Regression) ||
		(a.Duration != nil && a.Duration.Regression)
}

// HasComparison reports whether at least one metric could be compared.
func (a *Analysis) HasComparison() bool {
	if a == nil {
		return false
	}
	return (a.Size != nil && a.Size.Comparable) ||
		(a.Duration != nil && a.Duration.Comparable)
}

// Analyze compares current against the previous comparable builds in history
// (newest first, excluding the current build itself) using cfg thresholds.
//
// Invariants:
//   - duration is compared ONLY across identical cache modes and contexts;
//   - binary size ignores cache state entirely;
//   - incompatible platforms/toolchains/artifact types are filtered out;
//   - empty/nil metadata never produces a false regression;
//   - fewer than cfg.HistorySize previous builds is fine (baseline uses what
//     exists; a single previous build still yields Previous/Delta).
func Analyze(current *Report, history []HistoryEntry, cfg Config) *Analysis {
	a := &Analysis{
		HistoryTotal: len(history),
		Size:         &MetricComparison{SkipReason: "current build has no comparable value"},
		Duration:     &MetricComparison{SkipReason: "current build has no comparable value"},
	}
	if current == nil {
		return a
	}
	if cfg.HistorySize <= 0 {
		cfg.HistorySize = DefaultHistorySize
	}

	for _, h := range history {
		if h.Report == nil {
			a.SkippedNoMetadata++
		}
	}

	if current.Artifact.Type == ArtifactBinary || current.Artifact.Type == "" {
		a.Size = analyzeMetric(current, history, cfg,
			binarySizeSelector{}, cfg.BinarySizeAbsoluteThreshold,
			cfg.BinarySizePercentThreshold, directThresholdRule{})
	} else {
		a.Size = &MetricComparison{Comparable: false,
			SkipReason: "current artifact type has no binary-size comparison"}
	}
	a.Duration = analyzeMetric(current, history, cfg,
		durationSelector{}, int64(cfg.DurationAbsoluteThreshold.Milliseconds()),
		cfg.DurationPercentThreshold, durationThresholdRule{})
	return a
}

// FormatBytes renders a byte count using human units ("14.8 MB", "512 KB").
func FormatBytes(n int64) string {
	const kb, mb = 1024, 1024 * 1024
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// FormatDurationSeconds renders a millisecond count like "31.2s".
func FormatDurationSeconds(ms int64) string {
	return fmt.Sprintf("%.1fs", float64(ms)/1000.0)
}

// CacheLabel renders COLD/HIT/UNKNOWN style labels for CLI output.
func CacheLabel(s CacheStatus) string {
	switch s.Normalized() {
	case CacheCold:
		return "COLD"
	case CacheHit:
		return "HIT"
	default:
		return "UNKNOWN"
	}
}

// --- metric machinery -------------------------------------------------------

// thresholdRule decides whether a positive delta counts as a regression.
type thresholdRule interface {
	regression(delta, prev, absThr int64, pctThr float64) bool
}

type directThresholdRule struct{}

// direct: alert when absolute OR percentage growth crosses its threshold.
func (directThresholdRule) regression(delta, prev, absThr int64, pctThr float64) bool {
	if delta <= 0 || prev <= 0 {
		return false
	}
	pct := float64(delta) / float64(prev) * 100
	return delta >= absThr || pct >= pctThr
}

type durationThresholdRule struct{}

// duration: alert when relative slowdown exceeds the percent threshold AND
// the absolute change is beyond the noise floor, so tiny-but-highly-relative
// fluctuations never become warnings.
func (durationThresholdRule) regression(delta, prev, absThr int64, pctThr float64) bool {
	if delta <= 0 || prev <= 0 {
		return false
	}
	pct := float64(delta) / float64(prev) * 100
	return pct >= pctThr && delta >= absThr
}

// metricSelector extracts/validates one metric from a report. poolOK decides
// membership of the context-comparable candidate pool (identity match); the
// metric-specific cache gate is applied afterwards so a cache mismatch against
// the immediately-previous build surfaces as an explicit skip instead of
// silently comparing against an older build.
type metricSelector interface {
	value(r *Report) (int64, bool)
	poolOK(prev, cur *Report) (bool, string)
	cacheGate(prev, cur *Report) (bool, string)
}

// binarySizeSelector ignores cache state entirely (valid across cache modes).
type binarySizeSelector struct{}

func (binarySizeSelector) value(r *Report) (int64, bool) {
	if r == nil || r.Artifact.SizeBytes <= 0 {
		return 0, false
	}
	return r.Artifact.SizeBytes, true
}

func (s binarySizeSelector) poolOK(prev, cur *Report) (bool, string) {
	ok, reason := BinarySizeComparable(prev, cur)
	return ok, reason
}

func (binarySizeSelector) cacheGate(_, _ *Report) (bool, string) { return true, "" }

// durationSelector additionally requires identical, known cache modes for the
// direct comparison.
type durationSelector struct{}

func (durationSelector) value(r *Report) (int64, bool) {
	if r == nil || r.DurationMS <= 0 {
		return 0, false
	}
	return r.DurationMS, true
}

func (s durationSelector) poolOK(prev, cur *Report) (bool, string) {
	ok, reason := BinarySizeComparable(prev, cur)
	return ok, reason
}

func (durationSelector) cacheGate(prev, cur *Report) (bool, string) {
	p := prev.Cache.Status.Normalized()
	c := cur.Cache.Status.Normalized()
	if p == CacheUnknown || c == CacheUnknown {
		return false, "cache information unavailable for one of the builds"
	}
	if diff, reason := cacheModeDiffers(prev.Cache.Status, cur.Cache.Status); diff {
		return false, reason
	}
	return true, ""
}

// cacheModeDiffers renders the explicit skip reason when the previous
// comparable build used another cache mode than current.
func cacheModeDiffers(prevMode, curMode CacheStatus) (bool, string) {
	if prevMode.Normalized() == curMode.Normalized() {
		return false, ""
	}
	return true, fmt.Sprintf("cache mode differs from the previous comparable build (%s → %s)",
		CacheLabel(prevMode), CacheLabel(curMode))
}

// analyzeMetric implements the shared pool/baseline machinery for one metric.
// history must be ordered newest-first and exclude the current build.
//
// Semantics:
//   - candidate pool = up to cfg.HistorySize previous builds matching the
//     artifact context with a usable value (cache state NOT consulted);
//   - direct Previous = the immediately-previous comparable build (pool[0]);
//     a duration cache mismatch skips the whole duration comparison with an
//     explicit reason (never falls back to an older, stale build);
//   - baseline average spans the whole pool (duration baselines therefore
//     exist only when every pooled build shares the current cache mode,
//     avoiding blended cold/hit averages).
func analyzeMetric(current *Report, history []HistoryEntry, cfg Config,
	sel metricSelector, absThr int64, pctThr float64, rule thresholdRule) *MetricComparison {

	curVal, ok := sel.value(current)
	mc := &MetricComparison{Current: curVal}
	if !ok {
		mc.SkipReason = "current build has no comparable value"
		return mc
	}

	type matched struct {
		val    int64
		report *Report
	}
	var pool []matched
	for _, h := range history {
		if len(pool) >= cfg.HistorySize {
			break
		}
		if h.Report == nil {
			continue
		}
		v, valid := sel.value(h.Report)
		if !valid {
			continue
		}
		if cmpOK, _ := sel.poolOK(h.Report, current); !cmpOK {
			continue
		}
		pool = append(pool, matched{val: v, report: h.Report})
	}

	if len(pool) == 0 {
		mc.Comparable = false
		mc.SkipReason = "no previous comparable build found"
		return mc
	}

	prev := pool[0]
	if gateOK, gateReason := sel.cacheGate(prev.report, current); !gateOK {
		mc.Comparable = false
		mc.SkipReason = gateReason
		mc.HistoryUsed = len(pool)
		return mc
	}

	mc.Comparable = true
	mc.Previous = prev.val
	mc.Delta = curVal - prev.val
	if prev.val > 0 {
		mc.PercentDelta = float64(mc.Delta) / float64(prev.val) * 100
	} else {
		mc.PercentDelta = math.NaN()
	}
	mc.Regression = rule.regression(mc.Delta, prev.val, absThr, pctThr)
	mc.HistoryUsed = len(pool)

	var sum int64
	for _, m := range pool {
		sum += m.val
	}
	mc.BaselineAvg = sum / int64(len(pool))
	mc.BaselineDelta = curVal - mc.BaselineAvg
	if mc.BaselineAvg > 0 {
		mc.BaselinePctDelta = float64(mc.BaselineDelta) / float64(mc.BaselineAvg) * 100
	} else {
		mc.BaselinePctDelta = math.NaN()
	}
	return mc
}
