package matrix

import (
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// Canonical Matrix dimension names. A Combination is internally a set of
// dimension values; the typed fields on Combination (Lang, Version, OS, Arch,
// Variant) are the current concrete dimensions the engine supports. Adding a
// future dimension means adding a name here plus its normalization rule — the
// Run, scheduler, retry, and persistence layers operate on the dimension map
// and do not need to change.
const (
	DimLang    = "lang"
	DimVersion = "version"
	DimOS      = "os"
	DimArch    = "arch"
	DimVariant = "variant"
)

// dimensionNames is the closed set of dimension names the engine understands.
// Rule parsing rejects anything outside it (for exclude rules) or treats it as
// combination metadata (for include rules).
var dimensionNames = []string{DimLang, DimVersion, DimOS, DimArch, DimVariant}

// isDimensionName reports whether key names a supported Matrix dimension.
func isDimensionName(key string) bool {
	switch key {
	case DimLang, DimVersion, DimOS, DimArch, DimVariant:
		return true
	}
	return false
}

// DimensionNames returns the supported dimension names, sorted. It documents
// the model and feeds validation errors.
func DimensionNames() []string {
	out := append([]string(nil), dimensionNames...)
	sort.Strings(out)
	return out
}

// Dimensions is the generalized view of one combination: a set of dimension
// name → value pairs. It is a map, so all consumers must iterate the sorted
// keys (SortedKeys/Canonical) to stay deterministic — never the raw map order.
type Dimensions map[string]string

// SortedKeys returns the dimension names in sorted order.
func (d Dimensions) SortedKeys() []string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Canonical returns the deterministic dimension serialization:
// "arch=amd64/lang=go/os=linux/version=1.27" (sorted keys, "/"-joined).
func (d Dimensions) Canonical() string {
	parts := make([]string, 0, len(d))
	for _, k := range d.SortedKeys() {
		parts = append(parts, k+"="+d[k])
	}
	return strings.Join(parts, "/")
}

// Dimensions returns the generalized dimension view of the combination. The
// variant dimension is omitted when empty so identities of variant-less
// combinations are stable. The returned map is a copy.
func (c Combination) Dimensions() Dimensions {
	d := Dimensions{
		DimLang:    string(c.Lang),
		DimVersion: c.Version,
		DimOS:      c.OS,
		DimArch:    c.Arch,
	}
	if c.Variant != "" {
		d[DimVariant] = c.Variant
	}
	return d
}

// Identity returns the deterministic logical identity of the combination:
// the canonical dimension serialization, e.g.
// "arch=amd64/lang=go/os=linux/version=1.27". Unlike ID() (the human-readable,
// path-safe display form), Identity is the internal key for deduplication,
// include/exclude matching, and relating the same logical combination across
// different Matrix Runs.
func (c Combination) Identity() string {
	return c.Dimensions().Canonical()
}

// NewCombination builds a Combination from explicit dimension values,
// validating that the platform is known and the version well-formed. It is the
// constructor include rules use when they add combinations outside the base
// Cartesian product — the same validation the CLI/YAML paths apply.
func NewCombination(lang builder.Language, version, platform string) (Combination, error) {
	if lang != builder.Go && lang != builder.Rust {
		return Combination{}, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: unsupported language %q — supported: go, rust", lang)
	}
	cleanVersions, err := ValidateVersions(lang, []string{version})
	if err != nil {
		return Combination{}, err
	}
	cleanPlatforms, err := ValidatePlatforms([]string{platform})
	if err != nil {
		return Combination{}, err
	}
	parts := strings.SplitN(cleanPlatforms[0], "/", 3)
	comb := Combination{
		Lang:     lang,
		Version:  cleanVersions[0],
		OS:       parts[0],
		Arch:     parts[1],
		Platform: cleanPlatforms[0],
	}
	if len(parts) == 3 {
		comb.Variant = parts[2]
	}
	return comb, nil
}
