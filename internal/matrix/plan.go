package matrix

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// Combination represents a single build target in the matrix:
// one toolchain version × one platform pair.
type Combination struct {
	Lang     builder.Language // "go" or "rust"
	Version  string           // toolchain version, e.g. "1.22" or "1.22.4"
	OS       string           // e.g. "linux"
	Arch     string           // e.g. "amd64" or "arm" (never contains "/")
	Variant  string           // optional ARM variant ("v6"/"v7"); empty otherwise
	Platform string           // combined "os/arch[/variant]", e.g. "linux/arm/v7"
}

// ID returns a short stable identifier for this combination, suitable for
// use in cache keys, file names, and image tags. It is path-safe: every
// separator is a dash, including ARM variants ("go1.22.4-linux-arm-v7").
func (c Combination) ID() string {
	id := fmt.Sprintf("%s%s-%s-%s", c.Lang, c.Version, c.OS, c.Arch)
	if c.Variant != "" {
		id += "-" + c.Variant
	}
	return id
}

// BinaryName returns the output binary filename for this combination.
// Convention: {appname}_{arch}_{lang}_{version}[_{variant}]
// Example: test_amd64_go_1.26, test_arm_go_1.22.4_v7
func (c Combination) BinaryName(appName string) string {
	name := fmt.Sprintf("%s_%s_%s_%s", sanitizeNamePart(appName), c.Arch, c.Lang, c.Version)
	if c.Variant != "" {
		name += "_" + c.Variant
	}
	return name
}

// ImageTag returns the per-combination Docker image tag.
// Convention: {appname}:{lang}{version}-{arch}[-{variant}]
// Example: test:go1.26-amd64, test:go1.22.4-arm-v7
func (c Combination) ImageTag(appName string) string {
	return fmt.Sprintf("%s:%s", sanitizeNamePart(appName), c.DockerTagSuffix())
}

// DockerTagSuffix returns the combination-identifying part of an image tag,
// e.g. "go1.22.4-arm-v7" (no OS: Docker images are always linux).
func (c Combination) DockerTagSuffix() string {
	suffix := fmt.Sprintf("%s%s-%s", c.Lang, c.Version, c.Arch)
	if c.Variant != "" {
		suffix += "-" + c.Variant
	}
	return suffix
}

// sanitizeNamePart makes a user-supplied application name safe for use in
// file names and image references: path separators, colons and whitespace
// become dashes, and traversal sequences ("..") are removed.
func sanitizeNamePart(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ', '\t', '\n', '\r':
			return '-'
		}
		return r
	}, s)
	return strings.ReplaceAll(s, "..", "")
}

// MatrixPlan is the fully-expanded list of build combinations.
type MatrixPlan struct {
	Lang         builder.Language
	Combinations []Combination
}

// versionPattern accepts semantic toolchain versions with an optional patch
// component: "1.22", "1.22.4". A closed list of known versions would reject
// every patch release (and go stale the day a new minor ships).
var versionPattern = regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`)

// Well-known platforms. Kept intentionally small — users can extend as needed.
// Keys are lowercase; user input is normalized before matching.
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
// platform list. It validates every entry and returns an error on the first
// invalid one so the user can fix their flags before any builds start.
func ParsePlan(lang builder.Language, versions []string, platforms []string) (*MatrixPlan, error) {
	if lang != builder.Go && lang != builder.Rust {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: unsupported language %q — supported: go, rust", lang)
	}
	if len(versions) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: at least one %s version must be specified", lang)
	}
	if len(platforms) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: at least one platform must be specified")
	}

	cleanVersions, err := ValidateVersions(lang, versions)
	if err != nil {
		return nil, err
	}
	cleanPlatforms, err := ValidatePlatforms(platforms)
	if err != nil {
		return nil, err
	}

	// Expand into full cross product.
	combs := make([]Combination, 0, len(cleanVersions)*len(cleanPlatforms))
	for _, ver := range cleanVersions {
		for _, plat := range cleanPlatforms {
			parts := strings.SplitN(plat, "/", 3)
			comb := Combination{
				Lang:     lang,
				Version:  ver,
				OS:       parts[0],
				Arch:     parts[1],
				Platform: plat,
			}
			if len(parts) == 3 {
				comb.Variant = parts[2]
			}
			combs = append(combs, comb)
		}
	}

	return &MatrixPlan{
		Lang:         lang,
		Combinations: combs,
	}, nil
}

// ValidateVersions normalizes and checks one language's version list using the
// same rules ParsePlan applies to CLI flags. phelix.yaml matrix profiles route
// through it so YAML and CLI configuration are validated identically — there
// is no second validation path.
func ValidateVersions(lang builder.Language, versions []string) ([]string, error) {
	if len(versions) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: at least one %s version must be specified", lang)
	}
	cleanVersions := make([]string, 0, len(versions))
	for _, v := range versions {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		// Accept "go1.22", "rust1.77" or "v1.22" — strip the prefix.
		v = strings.TrimPrefix(v, "go")
		v = strings.TrimPrefix(v, "rust")
		v = strings.TrimPrefix(v, "v")
		if !versionPattern.MatchString(v) {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix: invalid %s version %q (expected major.minor or major.minor.patch, e.g. 1.22 or 1.22.4)", lang, v)
		}
		cleanVersions = append(cleanVersions, v)
	}
	if len(cleanVersions) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: at least one non-empty %s version must be specified", lang)
	}
	// De-duplicate while preserving order.
	return dedup(cleanVersions), nil
}

// ValidatePlatforms normalizes and checks the platform list using the same
// rules ParsePlan applies to CLI flags (see ValidateVersions).
func ValidatePlatforms(platforms []string) ([]string, error) {
	if len(platforms) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: at least one platform must be specified")
	}
	cleanPlatforms := make([]string, 0, len(platforms))
	for _, p := range platforms {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if !knownPlatforms[p] {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: unknown platform %q — known: %s",
				p, formatKnownPlatforms())
		}
		cleanPlatforms = append(cleanPlatforms, p)
	}
	if len(cleanPlatforms) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "matrix: at least one non-empty platform must be specified")
	}
	return dedup(cleanPlatforms), nil
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

func formatKnownPlatforms() string {
	return strings.Join(KnownPlatforms(), ", ")
}

// KnownPlatforms returns the supported platform list, sorted. It is the
// wizard's selectable platform menu and the documentation's answer to "which
// platforms can I configure".
func KnownPlatforms() []string {
	keys := make([]string, 0, len(knownPlatforms))
	for k := range knownPlatforms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
