package project

import (
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// --- Include/Exclude rule parsing ----------------------------------------------

func TestMatrixConfig_ParseIncludeExclude(t *testing.T) {
	cfg, err := loadMatrixYAML(t, `
matrix:
  enabled: true
  go:
    versions: ["1.25", "1.26", "1.27"]
  platforms:
    - linux/amd64
    - linux/arm64
    - windows/amd64
  retries: 2
  include:
    - go: "1.28"
      platform: linux/amd64
      tag: latest
  exclude:
    - go: 1.25
      platform: windows/amd64
`)
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Matrix
	if m.Retries != 2 {
		t.Fatalf("retries = %d", m.Retries)
	}
	if len(m.Include) != 1 || len(m.Exclude) != 1 {
		t.Fatalf("rules: include=%d exclude=%d", len(m.Include), len(m.Exclude))
	}

	// The bare `go: 1.25` scalar must survive the flat-map round-trip.
	exclude := m.Exclude[0].Fields()
	if exclude["go"] != "1.25" || exclude["platform"] != "windows/amd64" {
		t.Fatalf("exclude fields: %+v", exclude)
	}
	include := m.Include[0].Fields()
	if include["go"] != "1.28" || include["tag"] != "latest" {
		t.Fatalf("include fields: %+v", include)
	}
}

func TestMatrixConfig_MatrixProfileCarriesRules(t *testing.T) {
	cfg, err := loadMatrixYAML(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
  include:
    - go: "1.27"
      platform: linux/arm64
`)
	if err != nil {
		t.Fatal(err)
	}
	prof := cfg.Matrix.MatrixProfile()
	if len(prof.Include) != 1 {
		t.Fatalf("profile rules: include=%d", len(prof.Include))
	}
	inc := prof.Include[0]
	if inc.Dimensions[matrix.DimLang] != "go" || inc.Dimensions[matrix.DimVersion] != "1.27" {
		t.Fatalf("include rule: %+v", inc.Dimensions)
	}
	if inc.Metadata != nil {
		t.Fatalf("unexpected metadata: %+v", inc.Metadata)
	}
	if prof.Retries != 0 {
		t.Fatalf("retries = %d", prof.Retries)
	}

	// The profile expands through the real engine: 1×1 + 1 included = 2.
	plan, err := prof.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Combinations) != 2 || plan.IncludedCount != 1 {
		t.Fatalf("plan: %d combinations, %+v", len(plan.Combinations), plan)
	}
}

func TestMatrixConfig_RuleValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"exclude unknown dimension", `
matrix:
  enabled: true
  go: {versions: ["1.26"]}
  platforms: [linux/amd64]
  exclude:
    - cgo: "false"
`, "unknown dimension"},
		{"exclude empty value", `
matrix:
  enabled: true
  go: {versions: ["1.26"]}
  platforms: [linux/amd64]
  exclude:
    - go: ""
`, "empty value"},
		{"include empty rule", `
matrix:
  enabled: true
  go: {versions: ["1.26"]}
  platforms: [linux/amd64]
  include:
    - tag: "latest"
`, "matches no combination"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMatrixYAML(t, tc.body)
			if err == nil {
				t.Fatal("invalid rule accepted")
			}
			if !strings.Contains(err.Error(), tc.want) && !strings.Contains(err.Error(), "at least one dimension") {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestMatrixConfig_RuleWithoutDimensionsFails(t *testing.T) {
	_, err := loadMatrixYAML(t, `
matrix:
  enabled: true
  go: {versions: ["1.26"]}
  platforms: [linux/amd64]
  exclude:
    - {}
`)
	if err == nil {
		t.Fatal("empty exclude rule accepted")
	}
}

func TestMatrixConfig_NonScalarRuleValue(t *testing.T) {
	_, err := loadMatrixYAML(t, `
matrix:
  enabled: true
  go: {versions: ["1.26"]}
  platforms: [linux/amd64]
  exclude:
    - go:
        - 1.26
`)
	if err == nil {
		t.Fatal("non-scalar rule value accepted")
	}
}

func TestMatrixConfig_NegativeRetriesRejected(t *testing.T) {
	_, err := loadMatrixYAML(t, `
matrix:
  enabled: false
  go: {versions: ["1.26"]}
  platforms: [linux/amd64]
  retries: -1
`)
	if err == nil {
		t.Fatal("negative retries accepted")
	}
}

func TestSaveMatrixConfig_RoundTripRules(t *testing.T) {
	dir := t.TempDir()
	cfg := &MatrixConfig{
		Enabled:   true,
		Platforms: []string{"linux/amd64", "windows/amd64"},
		Retries:   1,
		Go:        &MatrixToolchain{Versions: []MatrixVersion{"1.25", "1.26", "1.27"}},
		Include:   []MatrixRule{MatrixRuleFromFields(map[string]string{"go": "1.28", "platform": "linux/amd64", "tag": "latest"})},
		Exclude:   []MatrixRule{MatrixRuleFromFields(map[string]string{"go": "1.25", "platform": "windows/amd64"})},
	}
	if err := SaveMatrixConfig(dir, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := reloaded.Matrix
	if m.Retries != 1 || len(m.Include) != 1 || len(m.Exclude) != 1 {
		t.Fatalf("round-trip lost fields: retries=%d include=%d exclude=%d", m.Retries, len(m.Include), len(m.Exclude))
	}
	inc := m.Include[0].Fields()
	if inc["go"] != "1.28" || inc["tag"] != "latest" {
		t.Fatalf("include round-trip: %+v", inc)
	}
}

// --- Profile convergence with rules (cmd-level contract, mirrored here) ------

func TestMatrixProfileRules_ExpandDeterministically(t *testing.T) {
	cfg, err := loadMatrixYAML(t, `
matrix:
  enabled: true
  go:
    versions: ["1.25", "1.26", "1.27"]
  platforms: [linux/amd64, linux/arm64, windows/amd64]
  exclude:
    - go: "1.25"
      platform: windows/amd64
`)
	if err != nil {
		t.Fatal(err)
	}
	prof := cfg.Matrix.MatrixProfile()
	first, err := prof.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Combinations) != 8 {
		t.Fatalf("combinations = %d, want 8 (9 - 1 excluded)", len(first.Combinations))
	}
	again, err := prof.Plan()
	if err != nil {
		t.Fatal(err)
	}
	for i := range first.Combinations {
		if first.Combinations[i].ID() != again.Combinations[i].ID() {
			t.Fatalf("order unstable at %d", i)
		}
	}
	if first.BaseCount != 9 || first.ExcludedCount != 1 {
		t.Fatalf("counts: %+v", first)
	}
	// Language flows through untouched.
	if prof.Lang != builder.Go {
		t.Fatalf("lang = %s", prof.Lang)
	}
}
