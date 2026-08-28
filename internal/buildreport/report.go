// Package buildreport collects structured build metrics after every native
// Go/Rust build and compares the current build against previous comparable
// builds to detect meaningful regressions.
//
// A Report is persisted as part of the existing version metadata
// (~/.phelix/apps/<AppName>/versions.json) — there is no separate build
// database. Regression analysis is pure observability: it never fails a build,
// never changes deploy behavior, and works fully offline.
package buildreport

import "time"

// CacheStatus classifies how a build interacted with the compiler's build
// cache. Missing metadata maps to CacheUnknown so consumers can distinguish
// "we don't know" from a real miss.
type CacheStatus string

const (
	// CacheCold means the compile performed real compilation work (cache miss
	// or first build).
	CacheCold CacheStatus = "cold"
	// CacheHit means the build was served from the compiler cache /
	// incremental state (cached or incremental build).
	CacheHit CacheStatus = "hit"
	// CacheUnknown means cache state could not be determined for this build.
	CacheUnknown CacheStatus = "unknown"
)

// Normalized returns the canonical lowercase status, mapping "" to unknown.
func (c CacheStatus) Normalized() CacheStatus {
	switch c {
	case CacheCold, CacheHit:
		return c
	default:
		return CacheUnknown
	}
}

// Artifact type values for ArtifactInfo.Type.
const (
	ArtifactBinary      = "binary"       // native Go/Rust binary artifact
	ArtifactDockerImage = "docker-image" // Docker image artifact (kept separate from binary metrics)
)

// Well-known cache source labels used by the builders.
const (
	CacheSourceGoBuild = "go-build-cache"    // Go standard build cache (GOCACHE)
	CacheSourceCargo   = "cargo-incremental" // Cargo incremental/target-dir cache
)

// CacheInfo describes the compiler-cache interaction of one build.
type CacheInfo struct {
	Status CacheStatus `json:"status"`
	// Source identifies which cache mechanism produced Status when known
	// (e.g. "go-build-cache", "cargo-incremental", "docker-layer"). Empty
	// when unavailable.
	Source string `json:"source,omitempty"`
}

// ArtifactInfo describes the produced artifact of one build.
type ArtifactInfo struct {
	// Type distinguishes artifact classes ("binary", "docker-image"). Binary
	// sizes are only ever compared against other binaries.
	Type string `json:"type"`
	// SizeBytes is the on-disk size of the artifact in bytes.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	// Platform is the target platform ("os/arch", e.g. "linux/amd64") when
	// applicable.
	Platform string `json:"platform,omitempty"`
}

// Report captures the structured metrics of a single build execution. It is
// embedded in the version metadata (deploy.VersionMeta.BuildReport) and every
// matrix artifact (deploy.MatrixArtifact.Report). The zero value with Failed
// unset is a valid successful report; all fields degrade gracefully when
// unavailable.
//
// Design note: application id/name, version number, tag and git commit are
// intentionally NOT duplicated here — they already live on the surrounding
// version metadata and reports are always stored together with it.
type Report struct {
	Language        string    `json:"language"`         // "go" | "rust"
	Compiler        string    `json:"compiler"`         // "go" | "rust/cargo"
	CompilerVersion string    `json:"compiler_version"` // normalized major.minor, e.g. "1.27"; may be empty when undetermined
	StartedAt       time.Time `json:"started_at,omitempty"`
	EndedAt         time.Time `json:"ended_at,omitempty"`
	// DurationMS is wall-clock build duration in milliseconds.
	DurationMS int64    `json:"duration_ms,omitempty"`
	BuildArgs  []string `json:"build_args,omitempty"`

	Cache    CacheInfo    `json:"cache"`
	Artifact ArtifactInfo `json:"artifact"`

	// Failure information. Successful builds leave Failed=false; failed builds
	// are currently reported on the terminal only (they must never become
	// versions), but the fields below keep the shape future-proof.
	Failed bool   `json:"failed,omitempty"`
	Stage  string `json:"stage,omitempty"` // failure stage, e.g. "compile"
	Error  string `json:"error,omitempty"` // sanitized failure summary
}

// IdentityKey returns a stable string identifying the comparable artifact
// context of this build: language + toolchain family/version + platform +
// artifact type. Two builds whose identity keys differ are never compared as
// if they were the same artifact (e.g. Go 1.27/linux-amd64 vs Go 1.26/arm64).
func (r *Report) IdentityKey() string {
	if r == nil {
		return ""
	}
	return r.Language + "|" + r.CompilerVersion + "|" + r.Artifact.Platform + "|" + r.Artifact.Type
}

// DurationSeconds renders DurationMS as fractional seconds.
func (r *Report) DurationSeconds() float64 {
	if r == nil {
		return 0
	}
	return float64(r.DurationMS) / 1000.0
}
