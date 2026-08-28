package builder

import (
	"regexp"
	"testing"

	"github.com/abdorrahmani/phelix/internal/buildreport"
)

func TestGoCacheStatusFromOutput(t *testing.T) {
	// `go build -v` prints nothing when every package is up to date → HIT.
	if got := GoCacheStatusFromOutput(""); got != buildreport.CacheHit {
		t.Fatalf("empty output should classify HIT, got %q", got)
	}
	// Any compiled-package line means real compilation → COLD.
	cold := GoCacheStatusFromOutput("github.com/user/pkg\n")
	if got := cold; got != buildreport.CacheCold {
		t.Fatalf("compiled-package output should classify COLD, got %q", got)
	}
	// Whitespace-only output still counts as fully cached.
	if got := GoCacheStatusFromOutput("   \n"); got != buildreport.CacheHit {
		t.Fatalf("whitespace output should classify HIT, got %q", got)
	}
}

func TestRustCacheStatusFromOutput(t *testing.T) {
	fresh := "    Finished `release` profile [optimized] target(s) in 0.42s\n"
	if got := RustCacheStatusFromOutput(fresh); got != buildreport.CacheHit {
		t.Fatalf("fresh cargo build should classify HIT, got %q", got)
	}
	compiling := "   Compiling serde v1.0.0\n    Finished `release` profile [optimized] target(s) in 12.3s\n"
	if got := RustCacheStatusFromOutput(compiling); got != buildreport.CacheCold {
		t.Fatalf("compiling output should classify COLD, got %q", got)
	}
	// Warnings alone do not count as compilation work.
	warn := "warning: unused variable\n    Finished dev profile\n"
	if got := RustCacheStatusFromOutput(warn); got != buildreport.CacheHit {
		t.Fatalf("warning-only output should classify HIT, got %q", got)
	}
}

func TestNormalizeToolchainVersion(t *testing.T) {
	cases := []struct {
		in   string
		re   *regexp.Regexp
		want string
	}{
		{"go version go1.27.0 linux/amd64\n", goVersionRe, "1.27"},
		{"go version go1.21.5 darwin/arm64\n", goVersionRe, "1.21"},
		{"rustc 1.85.0 (90b35a623 2025-01-03)\n", rustVersionRe, "1.85"},
		{"rustc 1.90.0\n", rustVersionRe, "1.90"},
		{"no version here", goVersionRe, ""},
	}
	for _, c := range cases {
		if got := normalizeToolchainVersion(c.in, c.re); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestObserveCacheNilTolerant(t *testing.T) {
	// observeCache must never panic on a nil observation sink.
	observeCache(BuildConfig{}, buildreport.CacheCold, buildreport.CacheSourceGoBuild)
}
