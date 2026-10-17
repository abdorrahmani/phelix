package builder

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
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

// The probe must honour the cache: a warm entry is returned without running
// the toolchain. Proven by planting an entry whose version the real toolchain
// would never report, then asserting that exact value comes back.
func TestDetectCompilerVersion_UsesCache(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	withTempCache(t)

	resolved, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		t.Fatal(err)
	}

	// 9.99 is not a real Go version: if it comes back, no subprocess ran.
	storeCachedVersion(Go, versionCacheEntry{
		Path:    resolved,
		Size:    fi.Size(),
		MtimeNS: fi.ModTime().UnixNano(),
		Version: "9.99",
	})

	if got := DetectCompilerVersion(Go); got != "9.99" {
		t.Fatalf("a warm entry must short-circuit the probe, got %q", got)
	}
}

// A cold cache must probe for real, write the entry, and report what the
// toolchain actually says.
func TestDetectCompilerVersion_ColdProbesAndCaches(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	withTempCache(t)

	got := DetectCompilerVersion(Go)
	if got == "" {
		t.Fatal("a present toolchain must report a version")
	}
	if !strings.HasPrefix(got, "1.") {
		t.Fatalf("expected a normalized major.minor like 1.27, got %q", got)
	}

	// The entry must now exist and echo the same version.
	data, err := os.ReadFile(cacheFilePath(Go))
	if err != nil {
		t.Fatalf("cold probe must write a cache entry: %v", err)
	}
	var e versionCacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("cache entry must be valid JSON: %v", err)
	}
	if e.Version != got {
		t.Fatalf("cache holds %q, probe returned %q", e.Version, got)
	}
	if e.Path == "" || e.MtimeNS == 0 {
		t.Fatalf("cache entry must record the binary identity, got %+v", e)
	}
}

// A stale entry must be ignored and replaced with the real version.
func TestDetectCompilerVersion_StaleEntryReplaced(t *testing.T) {
	resolved, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	withTempCache(t)

	real := DetectCompilerVersion(Go)
	if real == "" {
		t.Fatal("expected a version")
	}
	// Same path, wrong mtime: an in-place upgrade we have not seen.
	storeCachedVersion(Go, versionCacheEntry{
		Path: resolved, Size: 1, MtimeNS: 1, Version: "9.99"})

	if got := DetectCompilerVersion(Go); got != real {
		t.Fatalf("a stale entry must be replaced, got %q want %q", got, real)
	}
}

// An unsupported language must stay empty and never touch the cache.
func TestDetectCompilerVersion_UnsupportedLanguage(t *testing.T) {
	withTempCache(t)
	if got := DetectCompilerVersion(Language("cobol")); got != "" {
		t.Fatalf("an unsupported language must return empty, got %q", got)
	}
	// Nothing should have been written for a language that has no probe.
	if _, err := os.Stat(cacheFilePath(Language("cobol"))); err == nil {
		t.Fatal("an unsupported language must not write a cache entry")
	}
}

// A probe that yields no usable version must not record an entry. A junk entry
// would be skipped on read anyway, but writing one is still a bug: the cache
// file would claim a version no toolchain reported.
//
// Reaching this needs a binary that resolves but prints unrecognisable output.
// Emptying PATH is not enough, because that fails at LookPath and returns
// before the parse. A stub binary on PATH is the only way in.
func TestDetectCompilerVersion_NoEntryWhenOutputUnrecognisable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX-only")
	}
	withTempCache(t)

	dir := t.TempDir()
	stub := filepath.Join(dir, "go")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'not a version'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if got := DetectCompilerVersion(Go); got != "" {
		t.Fatalf("unrecognisable output must yield empty, got %q", got)
	}
	if _, err := os.Stat(cacheFilePath(Go)); err == nil {
		data, _ := os.ReadFile(cacheFilePath(Go))
		t.Fatalf("an unparsed version must not be cached, found: %s", data)
	}
}

// The same stub, but printing something valid: the entry must be written.
// Without this, the test above would also pass if caching never happened.
func TestDetectCompilerVersion_StubVersionIsCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX-only")
	}
	withTempCache(t)

	dir := t.TempDir()
	stub := filepath.Join(dir, "go")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'go version go1.99.0 linux/amd64'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if got := DetectCompilerVersion(Go); got != "1.99" {
		t.Fatalf("stub reports go1.99.0, expected 1.99, got %q", got)
	}
	data, err := os.ReadFile(cacheFilePath(Go))
	if err != nil {
		t.Fatalf("a parsed version must be cached: %v", err)
	}
	var e versionCacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	if e.Version != "1.99" {
		t.Fatalf("cache holds %q, want 1.99", e.Version)
	}

	// Second call must come from the cache. Make the stub report something
	// else while keeping it at the same path: the recorded path still matches,
	// but size and mtime now differ, so this proves the entry was actually
	// rewritten rather than merely re-probed.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'go version go1.98.0 linux/amd64'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DetectCompilerVersion(Go); got != "1.98" {
		t.Fatalf("a changed stub must invalidate the entry, got %q want 1.98", got)
	}
}
