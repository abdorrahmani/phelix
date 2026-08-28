package buildreport

import (
	"strings"
	"testing"
)

// helper: report factory for comparability tests
func repWith(lang, ver, platform string, size int64, durMS int64, cache CacheStatus) *Report {
	return &Report{
		Language:        lang,
		CompilerVersion: ver,
		DurationMS:      durMS,
		Cache:           CacheInfo{Status: cache},
		Artifact:        ArtifactInfo{Type: ArtifactBinary, SizeBytes: size, Platform: platform},
	}
}

// --- DurationComparable -----------------------------------------------------

func TestDurationComparable_ColdCold(t *testing.T) {
	prev := repWith("go", "1.27", "linux/amd64", 1000, 24800, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", 1100, 31200, CacheCold)
	ok, reason := DurationComparable(prev, cur)
	if !ok {
		t.Fatalf("cold→cold must be comparable, got reason %q", reason)
	}
}

func TestDurationComparable_HitHit(t *testing.T) {
	prev := repWith("go", "1.27", "linux/amd64", 1000, 4400, CacheHit)
	cur := repWith("go", "1.27", "linux/amd64", 1050, 5100, CacheHit)
	ok, _ := DurationComparable(prev, cur)
	if !ok {
		t.Fatalf("hit→hit must be comparable")
	}
}

func TestDurationComparable_ColdToHitSkipped(t *testing.T) {
	prev := repWith("go", "1.27", "linux/amd64", 1000, 21400, CacheCold)
	cur := repWith("go", "1.27", "linux/amd64", 1010, 5200, CacheHit)
	ok, reason := DurationComparable(prev, cur)
	if ok {
		t.Fatalf("cold→hit duration comparison must be skipped (false-regression guard)")
	}
	want := "cache mode differs"
	if len(reason) == 0 || !contains(reason, want) {
		t.Fatalf("reason should mention %q, got %q", want, reason)
	}
}

func TestDurationComparable_HitToColdSkipped(t *testing.T) {
	prev := repWith("go", "1.27", "linux/amd64", 1000, 8000, CacheHit)
	cur := repWith("go", "1.27", "linux/amd64", 1020, 29000, CacheCold)
	ok, reason := DurationComparable(prev, cur)
	if ok {
		t.Fatalf("hit→cold duration comparison must be skipped")
	}
	if !contains(reason, "cache mode differs") {
		t.Fatalf("unexpected reason %q", reason)
	}
}

func TestDurationComparable_MissingCacheMetadataSafe(t *testing.T) {
	// Old versions carry no cache info (unknown status) — comparison must be
	// skipped safely rather than guessed.
	prev := repWith("go", "1.27", "linux/amd64", 1000, 8000, CacheUnknown)
	cur := repWith("go", "1.27", "linux/amd64", 1010, 9000, CacheCold)
	ok, reason := DurationComparable(prev, cur)
	if ok {
		t.Fatalf("missing cache metadata must not allow duration comparison")
	}
	if !contains(reason, "cache information unavailable") {
		t.Fatalf("unexpected reason %q", reason)
	}

	// nil reports are equally safe.
	if ok, _ := DurationComparable(nil, cur); ok {
		t.Fatalf("nil previous report must be incomparable")
	}
	if ok, _ := DurationComparable(cur, nil); ok {
		t.Fatalf("nil current report must be incomparable")
	}
}

func TestDurationComparable_DifferentContextInvalid(t *testing.T) {
	go127amd := repWith("go", "1.27", "linux/amd64", 1000, 5000, CacheCold)
	go126arm := repWith("go", "1.26", "linux/arm64", 1200, 6000, CacheCold)
	rustAmd := repWith("rust", "1.85", "linux/amd64", 1300, 7000, CacheCold)

	for _, prev := range []*Report{go126arm, rustAmd} {
		if ok, _ := DurationComparable(prev, go127amd); ok {
			t.Fatalf("different toolchain/platform context must never compare")
		}
	}
}

// --- BinarySizeComparable ---------------------------------------------------

func TestBinarySizeComparable_ValidAcrossCacheStates(t *testing.T) {
	// COLD → HIT remains valid for binary size: cache state is irrelevant.
	coldPrev := repWith("go", "1.27", "linux/amd64", 14785088, 21400, CacheCold)
	hitCur := repWith("go", "1.27", "linux/amd64", 15518924, 5200, CacheHit)
	ok, _ := BinarySizeComparable(coldPrev, hitCur)
	if !ok {
		t.Fatalf("binary size must remain comparable across differing cache states")
	}
}

func TestBinarySizeComparable_IncompatibleContextsExcluded(t *testing.T) {
	base := repWith("go", "1.27", "linux/amd64", 15518924, 8000, CacheHit)

	cases := map[string]*Report{
		"different toolchain": repWith("go", "1.26", "linux/amd64", 14000000, 8000, CacheHit),
		"different platform":  repWith("go", "1.27", "linux/arm64", 15000000, 8000, CacheHit),
		"different language":  repWith("rust", "1.85", "linux/amd64", 16000000, 8000, CacheHit),
		"docker artifact":     repWith("go", "1.27", "linux/amd64", 0, 8000, CacheHit).withArtifactType(ArtifactDockerImage),
		"nil report":          nil,
	}
	for name, prev := range cases {
		if ok, _ := BinarySizeComparable(prev, base); ok {
			t.Fatalf("%s must not be binary-size comparable to base", name)
		}
	}
}

func (r *Report) withArtifactType(ty string) *Report {
	r.Artifact.Type = ty
	return r
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
