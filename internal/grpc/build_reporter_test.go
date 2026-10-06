package grpc

import (
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/buildreport"
)

// These tests pin the build-report wire mapping: every field the backend reads
// must be set, the error summary must be redacted (never a raw secret), and an
// absent report/analysis must stay nil rather than fabricating an empty one.

func TestToProtoBuildReport(t *testing.T) {
	t.Run("maps every field", func(t *testing.T) {
		started := time.Unix(1_700_000_000, 0)
		ended := started.Add(31200 * time.Millisecond)
		got := toProtoBuildReport(&buildreport.Report{
			Language:        "go",
			Compiler:        "go",
			CompilerVersion: "1.27",
			StartedAt:       started,
			EndedAt:         ended,
			DurationMS:      31200,
			BuildArgs:       []string{"build", "-trimpath"},
			Cache:           buildreport.CacheInfo{Status: buildreport.CacheHit, Source: buildreport.CacheSourceGoBuild},
			Artifact:        buildreport.ArtifactInfo{Type: buildreport.ArtifactBinary, SizeBytes: 15_500_000, Platform: "linux/amd64"},
			Failed:          true,
			Stage:           "compile",
			Error:           "compile failed: missing return at line 42",
		})
		if got == nil {
			t.Fatal("report must convert to a non-nil wire message")
		}
		// __REPORT_ASSERT_PLACEHOLDER__
		if got.GetLanguage() != "go" || got.GetCompiler() != "go" || got.GetCompilerVersion() != "1.27" {
			t.Fatalf("toolchain = %q/%q/%q", got.GetLanguage(), got.GetCompiler(), got.GetCompilerVersion())
		}
		if got.GetStartedAt() != started.UnixMilli() || got.GetEndedAt() != ended.UnixMilli() {
			t.Fatalf("timestamps = %d/%d, want %d/%d", got.GetStartedAt(), got.GetEndedAt(), started.UnixMilli(), ended.UnixMilli())
		}
		if got.GetDurationMs() != 31200 {
			t.Fatalf("duration = %d, want 31200", got.GetDurationMs())
		}
		if args := got.GetBuildArgs(); len(args) != 2 || args[0] != "build" || args[1] != "-trimpath" {
			t.Fatalf("build args = %v", args)
		}
		if got.GetCacheStatus() != "hit" || got.GetCacheSource() != buildreport.CacheSourceGoBuild {
			t.Fatalf("cache = %q/%q", got.GetCacheStatus(), got.GetCacheSource())
		}
		if got.GetArtifactType() != "binary" || got.GetArtifactSizeBytes() != 15_500_000 || got.GetArtifactPlatform() != "linux/amd64" {
			t.Fatalf("artifact = %q/%d/%q", got.GetArtifactType(), got.GetArtifactSizeBytes(), got.GetArtifactPlatform())
		}
		if !got.GetFailed() || got.GetStage() != "compile" {
			t.Fatalf("failure = failed:%v stage:%q", got.GetFailed(), got.GetStage())
		}
		if got.GetError() != "compile failed: missing return at line 42" {
			t.Fatalf("error = %q, want the safe summary unchanged", got.GetError())
		}
	})

	t.Run("error summary is redacted", func(t *testing.T) {
		got := toProtoBuildReport(&buildreport.Report{
			Language: "go", Failed: true, Stage: "compile",
			Error: "fatal: remote rejected: Authorization: Bearer eyJhbGciOiJIUzI1NiJ9",
		})
		if strings.Contains(got.GetError(), "eyJhbGciOiJIUzI1NiJ9") {
			t.Fatalf("a credential-looking token must never reach the wire, got %q", got.GetError())
		}
		if !strings.Contains(got.GetError(), "***") {
			t.Fatalf("redaction marker missing from %q", got.GetError())
		}
	})

	t.Run("nil is nil", func(t *testing.T) {
		if got := toProtoBuildReport(nil); got != nil {
			t.Fatalf("nil report must convert to nil, got %+v", got)
		}
	})
}

func TestToProtoBuildRegression(t *testing.T) {
	// Binary size ignores cache state, so it stays comparable even when the
	// duration comparison is skipped for a cache-mode mismatch. This single case
	// pins both the size deltas/baseline AND the skip-reason propagation.
	t.Run("size deltas and baseline map, duration skip reason propagates", func(t *testing.T) {
		got := toProtoBuildRegression(&buildreport.Analysis{
			Size: &buildreport.MetricComparison{
				Comparable: true, Previous: 1000, Current: 1200, Delta: 200,
				PercentDelta: 20, Regression: true, HistoryUsed: 3, BaselineAvg: 1100,
			},
			Duration: &buildreport.MetricComparison{
				Comparable: false, Current: 5500, HistoryUsed: 2,
				SkipReason: "cache mode differs from the previous comparable build (COLD → HIT)",
			},
		})
		if got == nil {
			t.Fatal("an analysis with a comparable metric must convert to a non-nil message")
		}
		if !got.GetSizeComparable() || got.GetSizePreviousBytes() != 1000 || got.GetSizeCurrentBytes() != 1200 {
			t.Fatalf("size prev/cur = comparable:%v %d/%d", got.GetSizeComparable(), got.GetSizePreviousBytes(), got.GetSizeCurrentBytes())
		}
		if got.GetSizeDeltaBytes() != 200 || got.GetSizePercentDelta() != 20 || !got.GetSizeRegression() {
			t.Fatalf("size delta = %d (%.1f%%) regression:%v", got.GetSizeDeltaBytes(), got.GetSizePercentDelta(), got.GetSizeRegression())
		}
		if got.GetBaselineSizeBytes() != 1100 || got.GetHistoryUsed() != 3 {
			t.Fatalf("size baseline/history = %d/%d, want 1100/3", got.GetBaselineSizeBytes(), got.GetHistoryUsed())
		}
		if got.GetDurationComparable() || got.GetDurationCurrentMs() != 5500 {
			t.Fatalf("duration comparable:%v current:%d", got.GetDurationComparable(), got.GetDurationCurrentMs())
		}
		if got.GetSkipReason() != "cache mode differs from the previous comparable build (COLD → HIT)" {
			t.Fatalf("skip reason = %q", got.GetSkipReason())
		}
	})
	// __REGRESSION_PLACEHOLDER__
	t.Run("duration deltas and baseline map when comparable", func(t *testing.T) {
		got := toProtoBuildRegression(&buildreport.Analysis{
			Duration: &buildreport.MetricComparison{
				Comparable: true, Previous: 5000, Current: 5500, Delta: 500,
				PercentDelta: 10, Regression: false, HistoryUsed: 4, BaselineAvg: 5200,
			},
		})
		if got == nil {
			t.Fatal("a comparable duration metric must convert to a non-nil message")
		}
		if !got.GetDurationComparable() || got.GetDurationPreviousMs() != 5000 || got.GetDurationCurrentMs() != 5500 {
			t.Fatalf("duration prev/cur = comparable:%v %d/%d", got.GetDurationComparable(), got.GetDurationPreviousMs(), got.GetDurationCurrentMs())
		}
		if got.GetDurationDeltaMs() != 500 || got.GetDurationPercentDelta() != 10 || got.GetDurationRegression() {
			t.Fatalf("duration delta = %d (%.1f%%) regression:%v", got.GetDurationDeltaMs(), got.GetDurationPercentDelta(), got.GetDurationRegression())
		}
		if got.GetBaselineDurationMs() != 5200 || got.GetHistoryUsed() != 4 {
			t.Fatalf("duration baseline/history = %d/%d, want 5200/4", got.GetBaselineDurationMs(), got.GetHistoryUsed())
		}
	})

	t.Run("nil and empty analyses are nil", func(t *testing.T) {
		if got := toProtoBuildRegression(nil); got != nil {
			t.Fatalf("nil analysis must convert to nil, got %+v", got)
		}
		if got := toProtoBuildRegression(&buildreport.Analysis{}); got != nil {
			t.Fatalf("an analysis with no comparable metric must convert to nil, got %+v", got)
		}
	})
}
