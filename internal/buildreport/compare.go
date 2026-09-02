package buildreport

// DurationComparable reports whether two builds may be compared on build
// duration. Duration is highly sensitive to build conditions, so a comparison
// is only valid when:
//
//  1. both builds produced comparable artifacts (same identity key), and
//  2. both cache modes are known and identical (COLD→COLD or HIT→HIT).
//
// COLD→HIT and HIT→COLD comparisons are rejected — a cold build compiles
// everything while a cached build mostly relinks, so a naive percentage would
// report false regressions ("build became 262% slower") caused purely by
// differing conditions. Cache state is intentionally checked ONLY here and
// never inside BinarySizeComparable.
func DurationComparable(prev, cur *Report) (bool, string) {
	if prev == nil || cur == nil {
		return false, "build metadata unavailable"
	}
	if prev.IdentityKey() != cur.IdentityKey() {
		return false, "artifact context differs from the previous comparable build"
	}
	p, c := prev.Cache.Status.Normalized(), cur.Cache.Status.Normalized()
	if p == CacheUnknown || c == CacheUnknown {
		return false, "cache information unavailable for one of the builds"
	}
	if p != c {
		return false, "cache mode differs from the previous comparable build"
	}
	return true, ""
}

// BinarySizeComparable reports whether two builds may be compared on binary /
// artifact size. Artifact size does not depend on compiler-cache state: an
// incremental (HIT) rebuild produces the same bytes as a COLD one when inputs
// are unchanged, so COLD→HIT remains valid. A size comparison only requires
// that the artifact context is comparable (same language, toolchain, target
// platform and artifact type).
func BinarySizeComparable(prev, cur *Report) (bool, string) {
	if prev == nil || cur == nil {
		return false, "build metadata unavailable"
	}
	if prev.IdentityKey() != cur.IdentityKey() {
		return false, "artifact context differs from the previous comparable build"
	}
	return true, ""
}
