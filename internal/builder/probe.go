package builder

import (
	"os/exec"
	"regexp"
	"strings"

	"github.com/abdorrahmani/phelix/internal/buildreport"
)

// GoCacheStatusFromOutput classifies a Go build as cache-cold or cache-hit
// using the output of `go build -v`: -v prints every package being compiled,
// so an empty result means all packages were served from the build cache
// (HIT / incremental relink), while any package lines mean real compilation
// work happened (COLD).
func GoCacheStatusFromOutput(output string) buildreport.CacheStatus {
	if strings.TrimSpace(output) == "" {
		return buildreport.CacheHit
	}
	return buildreport.CacheCold
}

// RustCacheStatusFromOutput classifies a Cargo build: fresh builds print only
// a "Finished" summary, whereas any "Compiling <crate>" line indicates real
// compilation work (COLD). Warnings alone keep the status HIT.
func RustCacheStatusFromOutput(output string) buildreport.CacheStatus {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "Compiling ") ||
			strings.HasPrefix(strings.TrimLeft(line, " "), "Building ") {
			return buildreport.CacheCold
		}
	}
	return buildreport.CacheHit
}

var (
	goVersionRe   = regexp.MustCompile(`go(\d+\.\d+)(\.\d+)?`)
	rustVersionRe = regexp.MustCompile(`rustc (\d+\.\d+)(\.\d+)?`)
)

// DetectCompilerVersion returns the normalized (major.minor) toolchain version
// for lang, e.g. "1.27" for Go or "1.85" for Rust. An empty string means the
// version could not be determined — callers must not fail a build over this.
//
// Cheap by design: one short-lived subprocess of an already-required
// toolchain binary, executed once per build.
func DetectCompilerVersion(lang Language) string {
	switch lang {
	case Go:
		out, err := exec.Command("go", "version").Output()
		if err != nil {
			return ""
		}
		return normalizeToolchainVersion(string(out), goVersionRe)
	case Rust:
		out, err := exec.Command("rustc", "--version").Output()
		if err != nil {
			// Fall back to the driver binary name used by some setups.
			return ""
		}
		return normalizeToolchainVersion(string(out), rustVersionRe)
	default:
		return ""
	}
}

// normalizeToolchainVersion extracts a "major.minor" version like "1.27"
// from raw toolchain output (e.g. "go version go1.27.0 linux/amd64" or
// "rustc 1.85.0 (…)"). Returns "" when nothing matches.
func normalizeToolchainVersion(output string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(output)
	if len(m) < 2 || m[1] == "" {
		return ""
	}
	return m[1]
}
