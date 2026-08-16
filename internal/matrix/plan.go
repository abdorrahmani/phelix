package matrix

import (
	"fmt"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// Combination represents a single build target in the matrix:
// one toolchain version × one platform pair.
type Combination struct {
	Lang     builder.Language // "go" or "rust"
	Version  string           // toolchain version, e.g. "1.22"
	OS       string           // e.g. "linux"
	Arch     string           // e.g. "amd64"
	Platform string           // combined "os/arch", e.g. "linux/amd64"
}

// ID returns a short stable identifier for this combination, suitable for
// use in cache keys, file names, and image tags.
func (c Combination) ID() string {
	return fmt.Sprintf("%s%s-%s", c.Lang, c.Version, c.Platform)
}

// BinaryName returns the output binary filename for this combination.
// Convention: {appname}_{arch}_{lang}_{version}
// Example: test_amd64_go_1.26
func (c Combination) BinaryName(appName string) string {
	return fmt.Sprintf("%s_%s_%s_%s", appName, c.Arch, c.Lang, c.Version)
}

// ImageTag returns the per-combination Docker image tag.
// Convention: {appname}:{lang}{version}-{arch}
// Example: test:go1.26-amd64
func (c Combination) ImageTag(appName string) string {
	return fmt.Sprintf("%s:%s%s-%s", appName, c.Lang, c.Version, c.Arch)
}

// MatrixPlan is the fully-expanded list of build combinations.
type MatrixPlan struct {
	Lang         builder.Language
	Combinations []Combination
}

// Well-known Go versions. Used for validation when no explicit list is given.
var knownGoVersions = map[string]bool{
	"1.20": true, "1.21": true, "1.22": true, "1.23": true, "1.24": true, "1.25": true, "1.26": true,
}

// Well-known Rust versions.
var knownRustVersions = map[string]bool{
	"1.75": true, "1.76": true, "1.77": true, "1.78": true, "1.79": true, "1.80": true, "1.90": true, "1.97": true,
}

// Well-known platforms. Kept intentionally small — users can extend as needed.
var knownPlatforms = map[string]bool{
	"linux/amd64":   true,
	"linux/arm64":   true,
	"linux/arm/v7":  true,
	"linux/arm/v6":  true,
	"darwin/amd64":  true,
	"darwin/arm64":  true,
	"windows/amd64": true,
}

// ParsePlan builds a MatrixPlan from the CLI-provided version lists and
// platform list. It validates every combination against known values and
// returns an error on the first invalid entry so the user can fix their
// flags before any builds start.
func ParsePlan(lang builder.Language, versions []string, platforms []string) (*MatrixPlan, error) {
	if len(versions) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: at least one %s version must be specified", lang)
	}
	if len(platforms) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: at least one platform must be specified")
	}

	knownVersions := knownGoVersions
	if lang == builder.Rust {
		knownVersions = knownRustVersions
	}

	// Validate and normalize versions.
	cleanVersions := make([]string, 0, len(versions))
	for _, v := range versions {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		// Accept "go1.22" or "1.22" — strip the "go" or "rust" prefix.
		v = strings.TrimPrefix(v, "go")
		v = strings.TrimPrefix(v, "rust")
		v = strings.TrimPrefix(v, "v")
		if !knownVersions[v] {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: unknown %s version %q — known: %s",
				lang, v, formatKnown(knownVersions))
		}
		cleanVersions = append(cleanVersions, v)
	}
	// De-duplicate while preserving order.
	cleanVersions = dedup(cleanVersions)

	// Validate and normalize platforms.
	cleanPlatforms := make([]string, 0, len(platforms))
	for _, p := range platforms {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !knownPlatforms[p] {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: unknown platform %q — known: %s",
				p, formatKnown(knownPlatforms))
		}
		cleanPlatforms = append(cleanPlatforms, p)
	}
	cleanPlatforms = dedup(cleanPlatforms)

	// Expand into full cross product.
	combs := make([]Combination, 0, len(cleanVersions)*len(cleanPlatforms))
	for _, ver := range cleanVersions {
		for _, plat := range cleanPlatforms {
			parts := strings.SplitN(plat, "/", 2)
			combs = append(combs, Combination{
				Lang:     lang,
				Version:  ver,
				OS:       parts[0],
				Arch:     parts[1],
				Platform: plat,
			})
		}
	}

	return &MatrixPlan{
		Lang:         lang,
		Combinations: combs,
	}, nil
}

// IsMatrixMode returns true when the user explicitly requested a matrix build.
// The caller passes the --matrix flag value and the version/platform slices.
func IsMatrixMode(matrixFlag bool, goVers, rustVers, platforms []string) bool {
	return matrixFlag || len(goVers) > 0 || len(rustVers) > 0 || len(platforms) > 0
}

// DetectLangForMatrix infers the language from project files when the user
// invokes --matrix without specifying a language. It delegates to the same
// detection logic used by the non-matrix build path.
func DetectLangForMatrix(projectRoot string) builder.Language {
	mgr := builder.NewBuildManager()
	return mgr.DetectLanguage(projectRoot)
}

func formatKnown(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
