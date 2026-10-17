package builder

import (
	"os"
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
	for line := range strings.SplitSeq(output, "\n") {
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

// toolchainProbe names the binary and argv that report a toolchain's version,
// plus the pattern extracting major.minor from its output.
type toolchainProbe struct {
	bin  string
	args []string
	re   *regexp.Regexp
}

// probeFor maps a language to its version probe.
func probeFor(lang Language) (toolchainProbe, bool) {
	switch lang {
	case Go:
		return toolchainProbe{
			bin:  "go",
			args: []string{"version"},
			re:   goVersionRe,
		}, true
	case Rust:
		return toolchainProbe{
			bin:  "rustc",
			args: []string{"--version"},
			re:   rustVersionRe,
		}, true
	}

	return toolchainProbe{}, false
}

// DetectCompilerVersion returns the normalized (major.minor) toolchain version
// for lang, e.g. "1.27" for Go or "1.85" for Rust. An empty string means the
// version could not be determined; callers must not fail a build over this.
func DetectCompilerVersion(lang Language) string {
	probe, ok := probeFor(lang)
	if !ok {
		return ""
	}

	resolved, err := exec.LookPath(probe.bin)
	if err != nil {
		return ""
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return ""
	}

	if v, ok := loadCachedVersion(lang, resolved, fi.Size(), fi.ModTime().UnixNano()); ok {
		return v
	}

	out, err := exec.Command(probe.bin, probe.args...).Output()
	if err != nil {
		return ""
	}
	version := normalizeToolchainVersion(string(out), probe.re)
	if version == "" {
		return ""
	}

	storeCachedVersion(lang, versionCacheEntry{
		Path:    resolved,
		Size:    fi.Size(),
		MtimeNS: fi.ModTime().UnixNano(),
		Version: version,
	})
	return version
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
