package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"go.yaml.in/yaml/v3"
)

// MatrixConfig is the phelix.yaml shape of a Matrix Profile — the persistent
// representation of the existing Build Matrix configuration:
//
//	matrix:
//	  enabled: true
//	  go:
//	    versions: ["1.26", "1.27"]
//	  platforms: [linux/amd64, linux/arm64]
//	  concurrency: 4
//	  retries: 2
//	  include:
//	    - go: "1.27"
//	      platform: linux/amd64
//	      tag: latest
//	  exclude:
//	    - go: "1.25"
//	      platform: windows/amd64
//
// The dimensions are exactly the ones the matrix engine supports (one
// ecosystem's toolchain versions × platforms × concurrency); no invented
// dimensions. Validation delegates to the same matrix.Validate* helpers the
// CLI path uses, so YAML and CLI configuration are validated identically.
type MatrixConfig struct {
	Enabled     bool             `yaml:"enabled"`
	Go          *MatrixToolchain `yaml:"go,omitempty"`
	Rust        *MatrixToolchain `yaml:"rust,omitempty"`
	Platforms   []string         `yaml:"platforms,omitempty"`
	Concurrency int              `yaml:"concurrency,omitempty"`
	// Retries is the automatic-retry budget for failed combinations
	// (N additional attempts per combination).
	Retries int          `yaml:"retries,omitempty"`
	Include []MatrixRule `yaml:"include,omitempty"`
	Exclude []MatrixRule `yaml:"exclude,omitempty"`
}

// MatrixRule is one include/exclude entry: a flat map whose keys are either
// dimension selectors (go/rust, lang, version, platform, os, arch, variant)
// or — for include rules only — metadata attributes attached to the matched
// combinations. Conversion and validation live in the matrix package
// (matrix.RuleFromFields / matrix.ValidateRule) so YAML rules and any future
// rule source behave identically.
type MatrixRule struct {
	fields map[string]string
}

// InvalidMatrixRuleValue is the sentinel stored for a non-scalar entry value.
// yaml v3 swallows custom-unmarshaler error messages, so a malformed value is
// reported by validate() with the yaml path instead of degrading to an opaque
// "malformed" error (same approach as MatrixVersion).
const InvalidMatrixRuleValue = "<invalid>"

// UnmarshalYAML implements yaml.Unmarshaler. It never fails; unparseable
// values become InvalidMatrixRuleValue, which MatrixConfig.validate reports.
func (r *MatrixRule) UnmarshalYAML(node *yaml.Node) error {
	r.fields = map[string]string{}
	if node.Kind != yaml.MappingNode {
		r.fields[""] = InvalidMatrixRuleValue
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if value.Kind != yaml.ScalarNode {
			r.fields[key.Value] = InvalidMatrixRuleValue
			continue
		}
		r.fields[key.Value] = strings.TrimSpace(value.Value)
	}
	return nil
}

// MarshalYAML implements yaml.Marshaler, writing the flat mapping so a saved
// file round-trips.
func (r MatrixRule) MarshalYAML() (any, error) {
	if r.fields == nil {
		return map[string]string{}, nil
	}
	return r.fields, nil
}

// Fields returns the raw key/value pairs of the rule (structural view; the
// matrix package decides which keys are dimensions).
func (r MatrixRule) Fields() map[string]string {
	if r.fields == nil {
		return nil
	}
	out := make(map[string]string, len(r.fields))
	for k, v := range r.fields {
		out[k] = v
	}
	return out
}

// MatrixRuleFromFields builds a MatrixRule from a flat field map (used by the
// wizard so its output is indistinguishable from a hand-written rule).
func MatrixRuleFromFields(fields map[string]string) MatrixRule {
	copied := make(map[string]string, len(fields))
	for k, v := range fields {
		copied[k] = v
	}
	return MatrixRule{fields: copied}
}

// MatrixToolchain is one ecosystem's version list.
type MatrixToolchain struct {
	Versions []MatrixVersion `yaml:"versions"`
}

// MatrixVersion is a toolchain version that accepts both quoted ("1.26") and
// bare (1.26) YAML scalars. yaml.v3 would otherwise reject a bare float when
// decoding into string, and version lists are exactly where users write bare
// numbers. Non-scalar entries become InvalidMatrixVersion, reported by
// validate() with the yaml path (yaml v3 swallows custom-unmarshaler error
// messages, so failing here would degrade to an opaque "malformed" error).
type MatrixVersion string

// InvalidMatrixVersion is the sentinel stored for an unparseable entry.
const InvalidMatrixVersion MatrixVersion = "<invalid>"

// UnmarshalYAML implements yaml.Unmarshaler. It never fails; unparseable
// entries become InvalidMatrixVersion, which MatrixConfig.validate reports.
func (v *MatrixVersion) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		*v = InvalidMatrixVersion
		return nil
	}
	*v = MatrixVersion(strings.TrimSpace(node.Value))
	return nil
}

// MarshalYAML implements yaml.Marshaler, writing the plain string so a saved
// file round-trips.
func (v MatrixVersion) MarshalYAML() (any, error) { return string(v), nil }

// validate checks the matrix section. Field-level errors name the exact yaml
// path; version/platform syntax goes through matrix.ValidateVersions /
// matrix.ValidatePlatforms — the same rules the CLI flags must satisfy.
func (m *MatrixConfig) validate() error {
	if m == nil {
		return nil
	}
	if m.Concurrency < 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: matrix.concurrency must be >= 1, got %d", m.Concurrency)
	}
	if m.Retries < 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"configuration error: matrix.retries must be >= 0, got %d", m.Retries)
	}

	for i, rule := range m.Include {
		for k, v := range rule.Fields() {
			if v == InvalidMatrixRuleValue {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: matrix.include[%d].%s is not a plain scalar value", i, k)
			}
		}
		if err := matrix.ValidateRuleFields("include", rule.Fields()); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
				"configuration error: matrix.include[%d]: %v", i, err)
		}
		if err := matrix.ValidateRule("include", matrix.RuleFromFields(rule.Fields())); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
				"configuration error: matrix.include[%d]: %v", i, err)
		}
	}
	for i, rule := range m.Exclude {
		for k, v := range rule.Fields() {
			if v == InvalidMatrixRuleValue {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: matrix.exclude[%d].%s is not a plain scalar value", i, k)
			}
		}
		if err := matrix.ValidateRuleFields("exclude", rule.Fields()); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
				"configuration error: matrix.exclude[%d]: %v", i, err)
		}
		if err := matrix.ValidateRule("exclude", matrix.RuleFromFields(rule.Fields())); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
				"configuration error: matrix.exclude[%d]: %v", i, err)
		}
	}

	goVersions := m.versionStrings(builder.Go)
	rustVersions := m.versionStrings(builder.Rust)

	if len(goVersions) > 0 && len(rustVersions) > 0 {
		return phelixerr.New(phelixerr.CodeConfiguration,
			"configuration error: matrix.go.versions and matrix.rust.versions are mutually exclusive — pick one ecosystem")
	}

	for _, ecos := range []struct {
		lang     builder.Language
		versions []string
	}{{builder.Go, goVersions}, {builder.Rust, rustVersions}} {
		for i, v := range ecos.versions {
			if MatrixVersion(v) == InvalidMatrixVersion {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: matrix.%s.versions[%d] is not a plain version string", ecos.lang, i)
			}
		}
		if len(ecos.versions) > 0 {
			if _, err := matrix.ValidateVersions(ecos.lang, ecos.versions); err != nil {
				// The reason travels in the message (the CLI renders the
				// context, not the wrapped cause) and in the cause chain
				// (errors.Is/As still reach it).
				return phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
					"configuration error: matrix.%s.versions: %v", ecos.lang, err)
			}
		}
	}

	if len(m.Platforms) > 0 {
		if _, err := matrix.ValidatePlatforms(m.Platforms); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
				"configuration error: matrix.platforms: %v", err)
		}
	}

	if !m.Enabled {
		return nil
	}
	if len(goVersions) == 0 && len(rustVersions) == 0 {
		return phelixerr.New(phelixerr.CodeConfiguration,
			"configuration error: matrix.go.versions (or matrix.rust.versions) must contain at least one version when matrix.enabled is true")
	}
	if len(m.Platforms) == 0 {
		return phelixerr.New(phelixerr.CodeConfiguration,
			"configuration error: matrix.platforms must contain at least one platform when matrix.enabled is true")
	}
	return nil
}

// versionStrings returns the configured version list for one ecosystem as
// plain strings (empty when that ecosystem is not configured).
func (m *MatrixConfig) versionStrings(lang builder.Language) []string {
	var tc *MatrixToolchain
	if lang == builder.Go {
		tc = m.Go
	} else {
		tc = m.Rust
	}
	if tc == nil {
		return nil
	}
	out := make([]string, 0, len(tc.Versions))
	for _, v := range tc.Versions {
		out = append(out, string(v))
	}
	return out
}

// MatrixProfile converts the YAML section into the engine's normalized
// matrix.Profile. A non-nil section always yields a profile; it may carry
// only some dimensions (e.g. platforms without versions) — matrix.Resolve
// merges it with CLI flags and defaults. The Enabled flag is intentionally
// dropped: it is a YAML concern the caller (matrix.Resolve) consumes
// separately. A nil section yields nil.
func (m *MatrixConfig) MatrixProfile() *matrix.Profile {
	if m == nil {
		return nil
	}
	p := &matrix.Profile{
		Platforms:   append([]string(nil), m.Platforms...),
		Concurrency: m.Concurrency,
		Retries:     m.Retries,
	}
	for _, rule := range m.Include {
		p.Include = append(p.Include, matrix.RuleFromFields(rule.Fields()))
	}
	for _, rule := range m.Exclude {
		p.Exclude = append(p.Exclude, matrix.RuleFromFields(rule.Fields()))
	}
	if versions := m.versionStrings(builder.Go); len(versions) > 0 {
		p.Lang = builder.Go
		p.Versions = versions
	} else if versions := m.versionStrings(builder.Rust); len(versions) > 0 {
		p.Lang = builder.Rust
		p.Versions = versions
	}
	return p
}

// SaveMatrixConfig writes the matrix section into dir/phelix.yaml while
// preserving every other key, comment, and the file's overall formatting: the
// existing document is edited as a yaml.Node and only the `matrix` mapping is
// replaced (or appended). A missing file is created with just the matrix
// section; a malformed existing file is an error, never overwritten.
func SaveMatrixConfig(dir string, mc *MatrixConfig) error {
	if mc == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "cannot save a nil matrix config")
	}
	if err := mc.validate(); err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)

	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", path)
		}
		// New file: the standard header plus just the matrix section.
		body, merr := yaml.Marshal(map[string]*MatrixConfig{"matrix": mc})
		if merr != nil {
			return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode matrix config", merr)
		}
		return os.WriteFile(path, append([]byte(matrixConfigHeader()), body...), 0o644)
	}

	var doc yaml.Node
	if uerr := yaml.Unmarshal(raw, &doc); uerr != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, uerr, "%s is malformed; not modifying it", path)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return phelixerr.Newf(phelixerr.CodeConfiguration, "%s is malformed: top level is not a mapping; not modifying it", path)
	}
	root := doc.Content[0]

	newValue := &yaml.Node{}
	if eerr := newValue.Encode(mc); eerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode matrix config", eerr)
	}

	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Kind == yaml.ScalarNode && root.Content[i].Value == "matrix" {
			// Keep the key node (its comments survive); swap only the value.
			*root.Content[i+1] = *newValue
			replaced = true
			break
		}
	}
	if !replaced {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "matrix"}
		root.Content = append(root.Content, key, newValue)
	}

	// Marshal the document node (not the root mapping): the file's leading
	// comments live on the document node and would be dropped otherwise.
	// Indent 2 keeps the common phelix.yaml style.
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if eerr := enc.Encode(&doc); eerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", eerr)
	}
	if cerr := enc.Close(); cerr != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode config", cerr)
	}
	return os.WriteFile(path, []byte(buf.String()), 0o644)
}

func matrixConfigHeader() string {
	return fmt.Sprintf("# Phelix project configuration.\n# CLI flags override these values (e.g. --port).\n")
}
