package matrix

import (
	"fmt"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// Rule is one include or exclude rule. Dimensions is a partial constraint set:
// a rule matches a combination when every constrained dimension equals the
// combination's value for it — unspecified dimensions match anything. Metadata
// (include rules only) is merged into matching combinations.
type Rule struct {
	// Dimensions holds the constrained dimensions. The keys are canonical
	// dimension names (see dimensions.go); the ecosystem shorthand ("go"/"rust"
	// keys in YAML) is resolved into lang+version by RuleFromFields.
	Dimensions Dimensions `json:"dimensions,omitempty"`
	// Metadata attaches key/value attributes to matched combinations.
	// Include rules only; excluded combinations never see it.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// RuleSet is the effective include/exclude configuration of a profile.
type RuleSet struct {
	Include []Rule `json:"include,omitempty"`
	Exclude []Rule `json:"exclude,omitempty"`
}

// ruleFieldGo / ruleFieldRust are the ecosystem shorthand keys accepted in
// rule maps: `go: "1.27"` means lang=go, version=1.27.
const (
	ruleFieldGo   = "go"
	ruleFieldRust = "rust"
)

// RuleFromFields converts one flat user-facing rule map (a YAML
// include/exclude entry) into a Rule. It is purely structural — no validation
// beyond normalization — so it cannot fail and can be used before deciding
// whether a rule is acceptable. Keys map as follows:
//
//	go|rust: <version>   → lang + version (mutually exclusive with each other)
//	lang: go|rust        → lang
//	version: <version>   → version
//	platform: os/arch    → os + arch (+ variant)
//	os / arch / variant  → individual dimensions
//
// Values are normalized (version prefixes stripped, platforms lowercased).
// Non-dimension keys are returned as metadata: include rules attach them to
// matched combinations, while ValidateRule rejects them for exclude rules
// (an exclude that silently ignores a typo'd key would match the wrong set).
func RuleFromFields(fields map[string]string) Rule {
	dims := map[string]string{}
	meta := map[string]string{}

	for k, v := range fields {
		switch k {
		case ruleFieldGo, ruleFieldRust:
			dims[DimLang] = k
			dims[DimVersion] = normalizeVersionValue(v)
		case DimLang:
			dims[DimLang] = strings.ToLower(strings.TrimSpace(v))
		case DimVersion:
			dims[DimVersion] = normalizeVersionValue(v)
		case "platform":
			p := strings.ToLower(strings.TrimSpace(v))
			parts := strings.SplitN(p, "/", 3)
			if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				dims[DimOS] = parts[0]
				dims[DimArch] = parts[1]
				if len(parts) == 3 && parts[2] != "" {
					dims[DimVariant] = parts[2]
				}
			} else {
				// Malformed platform: keep it on the platform key so
				// ValidateRule can report it instead of silently matching.
				dims["platform"] = p
			}
		case DimOS, DimArch, DimVariant:
			dims[k] = strings.ToLower(strings.TrimSpace(v))
		default:
			meta[k] = strings.TrimSpace(v)
		}
	}

	rule := Rule{}
	if len(dims) > 0 {
		rule.Dimensions = dims
	}
	if len(meta) > 0 {
		rule.Metadata = meta
	}
	return rule
}

// normalizeVersionValue applies the same prefix stripping ValidateVersions
// uses ("go1.22"/"rust1.77"/"v1.22" → "1.22") so rules and combinations
// compare equal regardless of how the user wrote the version.
func normalizeVersionValue(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "go")
	v = strings.TrimPrefix(v, "rust")
	v = strings.TrimPrefix(v, "v")
	return v
}

// ValidateRule checks one rule for the given kind. Requirements:
//
//   - at least one constrained dimension (an empty rule matches everything —
//     rejected so a typo cannot silently exclude/include the whole matrix);
//   - no empty dimension values;
//   - a well-formed platform value;
//   - for exclude rules: no metadata/unknown keys (they would be silently
//     ignored, hiding typos);
//   - lang, when present, must be go or rust.
//
// Well-formedness of versions is checked where the rule is applied
// (Expand), because only there is the ecosystem unambiguous.
func ValidateRule(kind string, rule Rule) error {
	label := "exclude"
	if kind == "include" {
		label = "include"
	}
	if len(rule.Dimensions) == 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: %s rule must constrain at least one dimension (got an empty rule)", label)
	}
	if kind != "include" && len(rule.Metadata) > 0 {
		keys := make([]string, 0, len(rule.Metadata))
		for k := range rule.Metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: %s rule has unknown dimension(s) %s — supported: go/rust, lang, version, platform, os, arch, variant",
			label, strings.Join(keys, ", "))
	}
	for _, k := range rule.Dimensions.SortedKeys() {
		if strings.TrimSpace(rule.Dimensions[k]) == "" {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix: %s rule has an empty value for dimension %q", label, k)
		}
	}
	if p, ok := rule.Dimensions["platform"]; ok {
		if !knownPlatforms[p] {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix: %s rule platform %q is malformed — expected os/arch[/variant], e.g. linux/amd64", label, p)
		}
	}
	if lang, ok := rule.Dimensions[DimLang]; ok && lang != string(builder.Go) && lang != string(builder.Rust) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: %s rule lang must be \"go\" or \"rust\", got %q", label, lang)
	}
	return nil
}

// Matches reports whether the rule's constraints are satisfied by the
// combination. Partial rules match every combination carrying the constrained
// values, regardless of the other dimensions.
func (r Rule) Matches(c Combination) bool {
	dims := c.Dimensions()
	for k, v := range r.Dimensions {
		if dims[k] != v {
			return false
		}
	}
	return true
}

// isFullRule reports whether an include rule constrains enough dimensions to
// identify (or create) one combination: ecosystem + version + platform.
// Partial rules can only merge metadata into existing combinations.
func (r Rule) isFullRule() bool {
	d := r.Dimensions
	return d[DimLang] != "" && d[DimVersion] != "" && d[DimOS] != "" && d[DimArch] != ""
}

// Describe renders a rule for error messages: "go=1.25 platform=windows/amd64".
func (r Rule) Describe() string {
	parts := make([]string, 0, len(r.Dimensions))
	for _, k := range r.Dimensions.SortedKeys() {
		if k == DimLang || k == DimVersion {
			continue // rendered as the ecosystem shorthand below
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, r.Dimensions[k]))
	}
	head := ""
	if r.Dimensions[DimLang] != "" {
		head = r.Dimensions[DimLang] + "=" + r.Dimensions[DimVersion]
	}
	if head == "" && r.Dimensions[DimVersion] != "" {
		head = "version=" + r.Dimensions[DimVersion]
	}
	if head != "" {
		parts = append([]string{head}, parts...)
	}
	return strings.Join(parts, " ")
}

// MergeMetadata merges src into dst's metadata map (include rules).
func (c *Combination) MergeMetadata(src map[string]string) {
	if len(src) == 0 {
		return
	}
	if c.Metadata == nil {
		c.Metadata = map[string]string{}
	}
	for k, v := range src {
		c.Metadata[k] = v
	}
}
