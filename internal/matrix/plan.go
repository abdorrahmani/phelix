package matrix

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// Combination represents a single build target in the matrix: one set of
// dimension values (see dimensions.go for the generalized model). The typed
// fields below are the dimensions the engine currently supports.
type Combination struct {
	Lang     builder.Language // "go" or "rust"
	Version  string           // toolchain version, e.g. "1.22" or "1.22.4"
	OS       string           // e.g. "linux"
	Arch     string           // e.g. "amd64" or "arm" (never contains "/")
	Variant  string           // optional ARM variant ("v6"/"v7"); empty otherwise
	Platform string           // combined "os/arch[/variant]", e.g. "linux/arm/v7"
	// Metadata carries attributes attached by include rules (e.g. tag: latest).
	// It never participates in the combination identity.
	Metadata map[string]string
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

// MatrixPlan is the fully-expanded list of build combinations. The counts
// explain how the final list came to be (base Cartesian product, combinations
// added by include rules, combinations removed by exclude rules) — the wizard
// preview and run inspection render them; execution only uses Combinations.
type MatrixPlan struct {
	Lang          builder.Language
	Combinations  []Combination
	BaseCount     int
	IncludedCount int
	ExcludedCount int
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
// platform list, without include/exclude rules. It is the legacy entry point
// (and the path the dockerize matrix uses); the profile path goes through
// Expand with the effective RuleSet.
func ParsePlan(lang builder.Language, versions []string, platforms []string) (*MatrixPlan, error) {
	return Expand(lang, versions, platforms, RuleSet{})
}

// Expand is the single matrix expansion engine. Pipeline (deterministic,
// documented, and identical for CLI, YAML, wizard, and future remote
// execution):
//
//	Base Cartesian product  (versions × platforms, input order preserved)
//	      ↓
//	Include rules           (merge metadata into matches, or append new
//	                         combinations; duplicates impossible by identity)
//	      ↓
//	Exclude rules           (partial matching; remove from the current list)
//	      ↓
//	Final combinations
//
// Every rule must match at least one combination at the point it is applied —
// a rule that silently matches nothing is treated as a configuration error so
// typos cannot quietly shrink or fail to shrink the matrix.
func Expand(lang builder.Language, versions []string, platforms []string, rules RuleSet) (*MatrixPlan, error) {
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

	for _, rule := range rules.Include {
		if err := ValidateRule("include", rule); err != nil {
			return nil, err
		}
	}
	for _, rule := range rules.Exclude {
		if err := ValidateRule("exclude", rule); err != nil {
			return nil, err
		}
	}

	// Base cross product, in input order — deterministic for the same
	// configuration.
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
	baseCount := len(combs)

	// Identity index — the dedupe and merge mechanism for include rules.
	byIdentity := make(map[string]int, len(combs))
	for i, c := range combs {
		byIdentity[c.Identity()] = i
	}

	// --- Include rules, in order.
	included := 0
	for _, rule := range rules.Include {
		if rule.isFullRule() {
			// A full rule names one combination (ecosystem + version +
			// platform): merge metadata into the existing combination, or
			// append it when the Cartesian product does not contain it.
			platform := rule.Dimensions[DimOS] + "/" + rule.Dimensions[DimArch]
			if v, ok := rule.Dimensions[DimVariant]; ok {
				platform += "/" + v
			}
			comb, cerr := NewCombination(builder.Language(rule.Dimensions[DimLang]), rule.Dimensions[DimVersion], platform)
			if cerr != nil {
				return nil, phelixerr.Wrapf(phelixerr.CodeInvalidArgument, cerr,
					"matrix: include rule [%s] is invalid", rule.Describe())
			}
			if idx, ok := byIdentity[comb.Identity()]; ok {
				combs[idx].MergeMetadata(rule.Metadata)
				continue
			}
			comb.MergeMetadata(rule.Metadata)
			byIdentity[comb.Identity()] = len(combs)
			combs = append(combs, comb)
			included++
			continue
		}
		// A partial rule can only attach metadata to combinations the base
		// matrix already contains; it must match at least one.
		matched := 0
		for i := range combs {
			if rule.Matches(combs[i]) {
				combs[i].MergeMetadata(rule.Metadata)
				matched++
			}
		}
		if matched == 0 {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix: include rule [%s] matches no combination — partial include rules can only add metadata to existing combinations",
				rule.Describe())
		}
	}

	// --- Exclude rules, in order. Each removes every current match; a rule
	// that matches nothing at its point in the pipeline is an error.
	excluded := 0
	for _, rule := range rules.Exclude {
		kept := combs[:0]
		removed := 0
		for _, c := range combs {
			if rule.Matches(c) {
				removed++
				continue
			}
			kept = append(kept, c)
		}
		if removed == 0 {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix: exclude rule [%s] matches no combination (it may have been removed by an earlier rule)", rule.Describe())
		}
		combs = kept
		excluded += removed
	}

	return &MatrixPlan{
		Lang:          lang,
		Combinations:  combs,
		BaseCount:     baseCount,
		IncludedCount: included,
		ExcludedCount: excluded,
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
