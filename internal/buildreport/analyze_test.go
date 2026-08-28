package buildreport

import (
	"math"
	"testing"
)

// hist builds HistoryEntry list from reports; version numbers count down 1..N
// (newest first).
func hist(reports ...*Report) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(reports))
	for i, r := range reports {
		ver := len(reports) - i
		if r == nil {
			out = append(out, HistoryEntry{Version: ver})
			continue
		}
		out = append(out, HistoryEntry{Version: ver, Report: r})
	}
	return out
}

const mb = int64(1024 * 1024)

func TestAnalyze_NoPreviousBuilds(t *testing.T) {
	cur := repWith("go", "1.27", "linux/amd64", 15*mb, 8000, CacheCold)
	a := Analyze(cur, nil, DefaultConfig())
	if a == nil || a.Size == nil || a.Duration == nil {
		t.Fatalf("analysis must always produce metric sections")
	}
	if a.Size.Comparable || a.Duration.Comparable {
		t.Fatalf("no history: comparisons must be skipped")
	}
	if a.Size.Regression || a.Duration.Regression {
		t.Fatalf("no history: no regression may be reported")
	}
}

func TestAnalyze_OnePreviousBuild(t *testing.T) {
	prev := repWith("go", "1.27", "linux/amd64", 14*mb+100*1024, 24800, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", 14*mb+850*1024, 31200, CacheCold) // +750KB (+5.19%)
	a := Analyze(cur, hist(prev), DefaultConfig())

	if !a.Size.Comparable {
		t.Fatalf("size comparison should be valid")
	}
	if got := a.Size.HistoryUsed; got != 1 {
		t.Fatalf("expected 1 comparable previous build, got %d", got)
	}
	wantDelta := (14*mb + 850*1024) - (14*mb + 100*1024)
	if a.Size.Delta != wantDelta {
		t.Fatalf("delta = %d, want %d", a.Size.Delta, wantDelta)
	}
	if math.Abs(a.Size.PercentDelta-5.19) > 0.1 {
		t.Fatalf("percent delta = %.2f, want ~5.19", a.Size.PercentDelta)
	}
	// +0.75MB < 1MB absolute but ≥5% relative → regression via OR rule.
	if !a.Size.Regression {
		t.Fatalf("percent-threshold crossing must flag binary size regression")
	}
}

func TestAnalyze_SizeDecreased_NoRegression(t *testing.T) {
	prev := repWith("go", "1.27", "linux/amd64", 16*mb, 20000, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", 15*mb, 19000, CacheCold)
	a := Analyze(cur, hist(prev), DefaultConfig())
	if !a.Size.Comparable {
		t.Fatal("comparable expected")
	}
	if a.Size.Delta >= 0 {
		t.Fatalf("expected negative delta, got %d", a.Size.Delta)
	}
	if a.Size.Regression {
		t.Fatal("a shrink is never a regression")
	}
}

func TestAnalyze_SizeUnchanged(t *testing.T) {
	prev := repWith("go", "1.27", "linux/amd64", 14*mb+100*1024, 20000, CacheHit)
	cur := repWith("go", "1.27", "linux/amd64", 14*mb+100*1024, 21000, CacheHit)
	a := Analyze(cur, hist(prev), DefaultConfig())
	if a.Size.Delta != 0 || a.Size.PercentDelta != 0 || a.Size.Regression {
		t.Fatalf("unchanged size: delta=%d pct=%.2f reg=%v",
			a.Size.Delta, a.Size.PercentDelta, a.Size.Regression)
	}
}

func TestAnalyze_BinaryAbsoluteThreshold(t *testing.T) {
	// +1.2MB on a 40MB base: ~3% (<5%) but above the 1MB absolute floor → alert.
	prev := repWith("go", "1.27", "linux/amd64", 40*mb, 20000, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", 40*mb+1200*1024, 21000, CacheCold)
	a := Analyze(cur, hist(prev), DefaultConfig())
	if !a.Size.Regression {
		t.Fatalf("+1.2MB (delta=%d) must cross the 1MB absolute threshold", a.Size.Delta)
	}
}

func TestAnalyze_BinaryBelowThresholds_NoAlert(t *testing.T) {
	// +0.7MB (<1MB) and +4.9% (<5%) → no regression, values still shown.
	prev := repWith("go", "1.27", "linux/amd64", 14*mb+100_000, 24800, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", prev.Artifact.SizeBytes+700_000, 31200, CacheCold)
	a := Analyze(cur, hist(prev), DefaultConfig())
	if a.Size.Regression {
		t.Fatalf("+0.7MB/+4.9%% must stay below thresholds (abs=%d, pct=%.2f)",
			a.Size.Delta, a.Size.PercentDelta)
	}
}

func TestAnalyze_DurationThresholdDetection(t *testing.T) {
	// Cold→cold: 24s → 32s = +33% (+8s): above percent and noise floor.
	prev := repWith("go", "1.27", "linux/amd64", 10*mb, 24000, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", 10*mb, 32000, CacheCold)
	a := Analyze(cur, hist(prev), DefaultConfig())
	if !a.Duration.Comparable {
		t.Fatalf("cold→cold duration must be comparable")
	}
	if !a.Duration.Regression {
		t.Fatalf("+8s (+33%%) must be flagged as time regression")
	}
}

func TestAnalyze_DurationSmallChange_NoNoise(t *testing.T) {
	// HIT→HIT with +0.4s on 4s (+10%): under both percent threshold and the
	// 2s noise floor → no warning.
	prev := repWith("go", "1.27", "linux/amd64", 10*mb, 4000, CacheHit)
	cur := repWith("go", "1.27", "linux/amd64", 10*mb, 4400, CacheHit)
	a := Analyze(cur, hist(prev), DefaultConfig())
	if a.Duration.Regression {
		t.Fatal("+0.4s micro-change must not trigger an alert")
	}
}

func TestAnalyze_DurationSkippedWhenCacheModesDiffer(t *testing.T) {
	coldPrev := repWith("go", "1.27", "linux/amd64", 10*mb, 21400, CacheCold)
	hitCur := repWith("go", "1.27", "linux/amd64", 10*mb+500_000, 5200, CacheHit)
	a := Analyze(hitCur, hist(coldPrev), DefaultConfig())

	if a.Duration.Comparable {
		t.Fatal("COLD→HIT duration comparison must be skipped")
	}
	if !contains(a.Duration.SkipReason, "cache mode differs") {
		t.Fatalf("skip reason should mention cache mode; got %q", a.Duration.SkipReason)
	}
	if a.Duration.PercentDelta != 0 && !math.IsNaN(a.Duration.PercentDelta) {
		if a.Duration.Previous != 0 || a.Duration.Current != hitCur.DurationMS {
			t.Fatalf("no misleading percentage may be produced for skipped comparison")
		}
	}
	// And size stays valid across the same boundary:
	if !a.Size.Comparable {
		t.Fatal("binary size remains comparable across cache modes")
	}
}

// --- history window ---------------------------------------------------------

func TestAnalyze_FewerThanFiveHistoricalBuilds(t *testing.T) {
	prev1 := repWith("go", "1.27", "linux/amd64", 14*mb, 20000, CacheHit)
	prev2 := repWith("go", "1.27", "linux/amd64", 14*mb+100_000, 21000, CacheHit)
	prev3 := repWith("go", "1.27", "linux/amd64", 14*mb+200_000, 22000, CacheHit)
	cur := repWith("go", "1.27", "linux/amd64", 14*mb+300_000, 23000, CacheHit)

	a := Analyze(cur, hist(prev3, prev2, prev1), DefaultConfig())
	if a.Size.HistoryUsed != 3 {
		t.Fatalf("expected 3 used builds, got %d", a.Size.HistoryUsed)
	}
	// immediate previous is prev3 (newest in list order).
	if a.Size.Previous != 14*mb+200_000 {
		t.Fatalf("previous must be the immediately-older comparable build; got %d", a.Size.Previous)
	}
	if want := int64((0 + 100_000 + 200_000) / 3); a.Size.BaselineAvg != 14*mb+want {
		t.Fatalf("baseline avg = %d, want %d", a.Size.BaselineAvg, 14*mb+want)
	}
}

func TestAnalyze_FiveBuildsExactly(t *testing.T) {
	var reports []*Report
	for i := 0; i < 5; i++ {
		reports = append(reports, repWith("go", "1.27", "linux/amd64",
			int64(10*mb)+int64(i)*100_000, int64(1000+i), CacheHit))
	}
	cur := repWith("go", "1.27", "linux/amd64", 11*mb, 1200, CacheHit)
	a := Analyze(cur, hist(reports...), DefaultConfig())
	if a.Size.HistoryUsed != 5 {
		t.Fatalf("expected 5, got %d", a.Size.HistoryUsed)
	}
	if a.Size.BaselineAvg != 10*mb+200_000 { // avg of offsets 0..400k
		t.Fatalf("baseline avg = %d", a.Size.BaselineAvg)
	}
}

func TestAnalyze_MoreThanFiveUsesLatestFive(t *testing.T) {
	var reports []*Report
	// hist() treats the first slice element as the NEWEST build.
	// Five recent builds (offsets 0..4) plus three old, much larger builds.
	// If the window ever reached past the five newest, the 30MB entries would
	// drag the baseline far away from 20MB.
	for i := 0; i < 5; i++ {
		reports = append(reports, repWith("go", "1.27", "linux/amd64",
			int64(20*mb)+int64(i), int64(900+i), CacheHit))
	}
	for i := 0; i < 3; i++ {
		reports = append(reports, repWith("go", "1.27", "linux/amd64",
			int64(30*mb), int64(500), CacheHit))
	}
	cur := repWith("go", "1.27", "linux/amd64", 21*mb, 1100, CacheHit)
	a := Analyze(cur, hist(reports...), DefaultConfig())
	if a.Size.HistoryUsed != 5 {
		t.Fatalf("window must cap at 5, got %d", a.Size.HistoryUsed)
	}
	if a.Size.BaselineAvg != 20*mb+2 { // avg of offsets 0..4
		t.Fatalf("avg over latest five = %d, want %d", a.Size.BaselineAvg, 20*mb+2)
	}
	if a.Size.Previous != 20*mb {
		t.Fatalf("immediately-previous build must be the newest in list; got %d", a.Size.Previous)
	}
}

func TestAnalyze_IncompatibleMatrixCombosExcluded(t *testing.T) {
	matching := repWith("go", "1.27", "linux/amd64", 15*mb, 21000, CacheHit)
	otherToolchain := repWith("go", "1.26", "linux/amd64", 30*mb, 5000, CacheHit)
	otherPlatform := repWith("go", "1.27", "linux/arm64", 32*mb, 5200, CacheHit)
	rustOther := repWith("rust", "1.85", "linux/amd64", 28*mb, 5600, CacheHit)

	cur := repWith("go", "1.27", "linux/amd64", 16*mb, 24000, CacheHit)
	a := Analyze(cur, hist(otherPlatform, rustOther, otherToolchain, matching), DefaultConfig())

	if !a.Size.Comparable {
		t.Fatal("matching combo present: comparison expected")
	}
	// Only the matching combo qualifies for the pool.
	if a.Size.HistoryUsed != 1 || a.Size.Previous != 15*mb {
		t.Fatalf("incompatible combos leaked into pool: used=%d prev=%d",
			a.Size.HistoryUsed, a.Size.Previous)
	}
	// No giant false deltas from foreign contexts (+1MB on 15MB ≈ 6.99%).
	if math.Abs(a.Size.PercentDelta-6.67) > 0.1 {
		t.Fatalf("unexpected percent delta %.2f — comparisons mixed contexts?", a.Size.PercentDelta)
	}
}

func TestAnalyze_OldVersionsWithoutMetadataSkipped(t *testing.T) {
	noMeta1 := HistoryEntry{Version: 9} // nil report
	noMeta2 := HistoryEntry{Version: 8}
	legacy := repWith("", "", "", 12*mb, 15000, CacheUnknown) // no identity info
	withData := repWith("go", "1.27", "linux/amd64", 14*mb, 22000, CacheHit)
	cur := repWith("go", "1.27", "linux/amd64", 14*mb+600*1024, 23500, CacheHit)

	a := Analyze(cur, []HistoryEntry{noMeta1, noMeta2, {Version: 7, Report: legacy}, {Version: 6, Report: withData}}, DefaultConfig())
	if a.SkippedNoMetadata != 2 {
		t.Fatalf("expected 2 metadata-less entries counted, got %d", a.SkippedNoMetadata)
	}
	if !a.Size.Comparable || a.Size.Previous != 14*mb {
		t.Fatalf("comparison against withData expected; got prev=%d used=%d",
			a.Size.Previous, a.Size.HistoryUsed)
	}
	// The version lacking identity info must not join pools:
	if a.Size.HistoryUsed != 1 {
		t.Fatalf("metadata-less/differing versions must be excluded from pool, got %d", a.Size.HistoryUsed)
	}
}

func TestAnalyze_DockerArtifactsNeverCompareAsBinary(t *testing.T) {
	dockerPrev := repWith("go", "1.27", "linux/amd64", 0, 40000, CacheUnknown).withArtifactType(ArtifactDockerImage)
	binaryCur := repWith("go", "1.27", "linux/amd64", 15*mb, 8000, CacheCold)

	a := Analyze(binaryCur, hist(dockerPrev), DefaultConfig())
	if a.Size.Comparable && a.Size.Regression {
		t.Fatal("docker image metrics must never feed native-binary regressions")
	}
}

func TestAnalyze_CurrentNilIsSafe(t *testing.T) {
	a := Analyze(nil, hist(repWith("go", "1.27", "linux/amd64", mb, 1000, CacheHit)), DefaultConfig())
	if a == nil || a.Size == nil || a.Duration == nil {
		t.Fatal("analysis should still return complete structure")
	}
	if a.AnyRegressed() {
		t.Fatal("nil current cannot regress")
	}
}

func TestFormatHelpers(t *testing.T) {
	if FormatBytes(15518924) != "14.8 MB" {
		t.Fatalf("FormatBytes = %q", FormatBytes(15518924))
	}
	if FormatDurationSeconds(31200) != "31.2s" {
		t.Fatalf("FormatDurationSeconds = %q", FormatDurationSeconds(31200))
	}
	if CacheLabel(CacheHit) != "HIT" || CacheLabel(CacheCold) != "COLD" ||
		CacheLabel(CacheUnknown) != "UNKNOWN" || CacheLabel("") != "UNKNOWN" {
		t.Fatal("cache label mapping broken")
	}
	if sb := signedBytes(700_000); sb[0] != '+' {
		t.Fatalf("positive delta must carry explicit plus: %q", sb)
	}
	if got := pctString(math.NaN()); got != "\u2014" {
		t.Fatalf("NaN pct must render em-dash, got %q", got)
	}
}
