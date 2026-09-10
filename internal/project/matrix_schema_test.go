package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// loadMatrixYAML writes the yaml body into a temp dir's phelix.yaml and loads it.
func loadMatrixYAML(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(dir)
}

// --- Parsing ---------------------------------------------------------------------

func TestMatrixConfig_ParseValid(t *testing.T) {
	cfg, err := loadMatrixYAML(t, `
name: myapp
port: 8080
matrix:
  enabled: true
  go:
    versions:
      - 1.26      # bare scalar on purpose — users write unquoted versions
      - "1.27"
  platforms:
    - linux/amd64
    - linux/arm64
  concurrency: 4
`)
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Matrix
	if m == nil || !m.Enabled {
		t.Fatalf("matrix section not parsed: %+v", m)
	}
	if len(m.Go.Versions) != 2 || string(m.Go.Versions[0]) != "1.26" || string(m.Go.Versions[1]) != "1.27" {
		t.Fatalf("versions: %v", m.Go.Versions)
	}
	if len(m.Platforms) != 2 || m.Platforms[1] != "linux/arm64" {
		t.Fatalf("platforms: %v", m.Platforms)
	}
	if m.Concurrency != 4 {
		t.Fatalf("concurrency: %d", m.Concurrency)
	}
}

func TestMatrixConfig_ParseRust(t *testing.T) {
	cfg, err := loadMatrixYAML(t, `
matrix:
  enabled: true
  rust:
    versions: [1.77, 1.78]
  platforms: [linux/amd64]
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Matrix.Rust == nil || len(cfg.Matrix.Rust.Versions) != 2 {
		t.Fatalf("rust section: %+v", cfg.Matrix.Rust)
	}
	if cfg.Matrix.Go != nil {
		t.Fatalf("go section should be absent, got %+v", cfg.Matrix.Go)
	}
}

func TestMatrixConfig_MissingAndDisabled(t *testing.T) {
	cfg, err := loadMatrixYAML(t, "name: myapp\nport: 8080\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Matrix != nil {
		t.Fatalf("no matrix section expected, got %+v", cfg.Matrix)
	}

	cfg, err = loadMatrixYAML(t, `
matrix:
  enabled: false
  go:
    versions: ["1.26"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Matrix.Enabled {
		t.Fatal("disabled matrix must load fine (syntax still validated)")
	}
}

// --- Validation (same rules as CLI flags) ------------------------------------------

func TestMatrixConfig_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			"enabled without versions",
			"matrix:\n  enabled: true\n  platforms: [linux/amd64]\n",
			"matrix.go.versions",
		},
		{
			"enabled without platforms",
			"matrix:\n  enabled: true\n  go:\n    versions: [\"1.26\"]\n",
			"matrix.platforms",
		},
		{
			"both ecosystems",
			"matrix:\n  enabled: true\n  go:\n    versions: [\"1.26\"]\n  rust:\n    versions: [\"1.77\"]\n  platforms: [linux/amd64]\n",
			"mutually exclusive",
		},
		{
			"invalid version",
			"matrix:\n  enabled: true\n  go:\n    versions: [\"latest\"]\n  platforms: [linux/amd64]\n",
			"invalid go version",
		},
		{
			"unknown platform",
			"matrix:\n  enabled: true\n  go:\n    versions: [\"1.26\"]\n  platforms: [linux/mips]\n",
			"unknown platform",
		},
		{
			"negative concurrency",
			"matrix:\n  enabled: true\n  go:\n    versions: [\"1.26\"]\n  platforms: [linux/amd64]\n  concurrency: -1\n",
			"matrix.concurrency",
		},
		{
			"empty version list",
			"matrix:\n  enabled: true\n  go:\n    versions: []\n  platforms: [linux/amd64]\n",
			"at least one version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMatrixYAML(t, tc.yaml)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name the config location %q", err, tc.want)
			}
		})
	}
}

func TestMatrixConfig_InvalidVersionSyntaxFailsEvenWhenDisabled(t *testing.T) {
	_, err := loadMatrixYAML(t, "matrix:\n  enabled: false\n  go:\n    versions: [\"one\"]\n")
	if err == nil {
		t.Fatal("syntax must be validated even when the matrix is disabled")
	}
}

func TestMatrixConfig_NonScalarVersionEntry(t *testing.T) {
	_, err := loadMatrixYAML(t, "matrix:\n  enabled: true\n  go:\n    versions:\n      - [1.26]\n  platforms: [linux/amd64]\n")
	if err == nil || !strings.Contains(err.Error(), "matrix.go.versions") {
		t.Fatalf("expected matrix.go.versions error, got %v", err)
	}
}

// --- Conversion to the engine profile ----------------------------------------------

func TestMatrixConfig_MatrixProfile(t *testing.T) {
	cfg, err := loadMatrixYAML(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26", "1.27"]
  platforms: [linux/amd64, linux/arm64]
  concurrency: 4
`)
	if err != nil {
		t.Fatal(err)
	}
	prof := cfg.Matrix.MatrixProfile()
	if prof == nil {
		t.Fatal("expected a profile")
	}
	if prof.Lang != builder.Go || len(prof.Versions) != 2 || len(prof.Platforms) != 2 || prof.Concurrency != 4 {
		t.Fatalf("profile: %+v", prof)
	}

	// Platform-only section: still a profile (Resolve merges it with CLI
	// flags); it just carries no versions.
	platformOnly, err := loadMatrixYAML(t, "matrix:\n  enabled: false\n  platforms: [linux/amd64]\n")
	if err != nil {
		t.Fatal(err)
	}
	if p := platformOnly.Matrix.MatrixProfile(); p == nil || len(p.Platforms) != 1 || len(p.Versions) != 0 {
		t.Fatalf("platform-only profile: %+v", p)
	}

	// Nil receiver safety.
	var nilCfg *MatrixConfig
	if p := nilCfg.MatrixProfile(); p != nil {
		t.Fatal("nil MatrixConfig must yield nil profile")
	}
}

// --- SaveMatrixConfig: preserve everything unrelated ---------------------------------

func TestSaveMatrixConfig_PreservesUnrelatedFieldsAndComments(t *testing.T) {
	dir := t.TempDir()
	original := `# Phelix project configuration.
# Keep this comment.

app_note: "handled elsewhere"  # trailing comment survives

name: myapp
port: 8080

health:
  endpoints:
    - name: default
      path: /health
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	mc := &MatrixConfig{
		Enabled:     true,
		Go:          &MatrixToolchain{Versions: []MatrixVersion{"1.26", "1.27"}},
		Platforms:   []string{"linux/amd64", "linux/arm64"},
		Concurrency: 4,
	}
	if err := SaveMatrixConfig(dir, mc); err != nil {
		t.Fatal(err)
	}

	saved, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)

	// Unrelated keys, values, and comments must survive verbatim.
	for _, want := range []string{
		"# Keep this comment.",
		`app_note: "handled elsewhere"`,
		"name: myapp",
		"port: 8080",
		"path: /health",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("saved file lost %q:\n%s", want, text)
		}
	}
	// The matrix section must be present.
	if !strings.Contains(text, "matrix:") || !strings.Contains(text, "enabled: true") {
		t.Fatalf("matrix section missing:\n%s", text)
	}

	// And the file must still load with the matrix section intact.
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("saved file no longer loads: %v", err)
	}
	if cfg.Name != "myapp" || cfg.Port != 8080 || cfg.Health == nil {
		t.Fatalf("unrelated config damaged: %+v", cfg)
	}
	if cfg.Matrix == nil || !cfg.Matrix.Enabled || cfg.Matrix.Concurrency != 4 {
		t.Fatalf("matrix section lost: %+v", cfg.Matrix)
	}
}

func TestSaveMatrixConfig_ReplacesExistingMatrixSection(t *testing.T) {
	dir := t.TempDir()
	original := `name: myapp
port: 8080

matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	mc := &MatrixConfig{
		Enabled:   true,
		Rust:      &MatrixToolchain{Versions: []MatrixVersion{"1.77"}},
		Platforms: []string{"linux/arm64"},
	}
	if err := SaveMatrixConfig(dir, mc); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Matrix.Go != nil {
		t.Fatalf("old go section must be replaced, got %+v", cfg.Matrix.Go)
	}
	if cfg.Matrix.Rust == nil || len(cfg.Matrix.Rust.Versions) != 1 {
		t.Fatalf("new rust section missing: %+v", cfg.Matrix.Rust)
	}
	if len(cfg.Matrix.Platforms) != 1 || cfg.Matrix.Platforms[0] != "linux/arm64" {
		t.Fatalf("platforms not replaced: %v", cfg.Matrix.Platforms)
	}
}

func TestSaveMatrixConfig_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	mc := &MatrixConfig{
		Enabled:   true,
		Go:        &MatrixToolchain{Versions: []MatrixVersion{"1.26"}},
		Platforms: []string{"linux/amd64"},
	}
	if err := SaveMatrixConfig(dir, mc); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Matrix == nil || !cfg.Matrix.Enabled {
		t.Fatalf("new file missing matrix section: %+v", cfg)
	}
	if cfg.Name != "" || cfg.Port != 0 {
		t.Fatalf("new file should not invent unrelated values: %+v", cfg)
	}
}

func TestSaveMatrixConfig_RejectsInvalidAndRefusesToTouchMalformed(t *testing.T) {
	dir := t.TempDir()

	// Invalid config → rejected before any write.
	invalid := &MatrixConfig{Enabled: true, Platforms: []string{"linux/amd64"}} // no versions
	if err := SaveMatrixConfig(dir, invalid); err == nil {
		t.Fatal("invalid matrix config must be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("no file should be created for an invalid config")
	}

	// Malformed existing file → error, file untouched.
	path := filepath.Join(dir, FileName)
	malformed := ":\n  - not a mapping"
	before, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(malformed), 0o644)
	valid := &MatrixConfig{Enabled: true, Go: &MatrixToolchain{Versions: []MatrixVersion{"1.26"}}, Platforms: []string{"linux/amd64"}}
	if err := SaveMatrixConfig(dir, valid); err == nil {
		t.Fatal("malformed phelix.yaml must not be silently modified")
	}
	after, _ := os.ReadFile(path)
	if string(before) != "" && string(after) != malformed {
		t.Fatal("malformed file was modified")
	}
}

func TestSaveMatrixConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	mc := &MatrixConfig{
		Enabled:     true,
		Go:          &MatrixToolchain{Versions: []MatrixVersion{"1.25", "1.26", "1.27"}},
		Platforms:   []string{"linux/amd64", "linux/arm64", "darwin/arm64"},
		Concurrency: 6,
	}
	if err := SaveMatrixConfig(dir, mc); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Matrix
	if len(got.Go.Versions) != 3 || string(got.Go.Versions[2]) != "1.27" {
		t.Fatalf("versions round-trip: %v", got.Go.Versions)
	}
	if len(got.Platforms) != 3 || got.Platforms[2] != "darwin/arm64" {
		t.Fatalf("platforms round-trip: %v", got.Platforms)
	}
	if got.Concurrency != 6 {
		t.Fatalf("concurrency round-trip: %d", got.Concurrency)
	}
}

// --- Ambiguous rule spellings ------------------------------------------------------

// A rule that mixes the ecosystem shorthand with an explicit lang/version key
// (or uses both shorthands) is ambiguous: conversion could only resolve it by
// arbitrary precedence. It must be reported as a configuration error naming
// the YAML location, not silently interpreted.
func TestMatrixConfig_AmbiguousRuleSpellingsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			"both shorthands",
			`
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
  exclude:
    - go: "1.25"
      rust: "1.77"
`,
		},
		{
			"shorthand plus lang",
			`
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
  include:
    - go: "1.28"
      lang: rust
      platform: linux/amd64
`,
		},
		{
			"shorthand plus version",
			`
matrix:
  enabled: true
  rust:
    versions: ["1.77"]
  platforms: [linux/amd64]
  include:
    - rust: "1.78"
      version: "1.79"
      platform: linux/amd64
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMatrixYAML(t, tc.body)
			if err == nil {
				t.Fatal("ambiguous rule spelling must be rejected")
			}
			if !strings.Contains(err.Error(), "configuration error: matrix.") {
				t.Fatalf("error must name the YAML location, got: %v", err)
			}
		})
	}
}
