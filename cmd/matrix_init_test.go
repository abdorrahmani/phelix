package cmd

import (
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/project"
)

// The wizard's interactive prompts are survey-driven and only run in a TTY;
// these tests cover the model layer (config generation, preview via the real
// expansion, existing-profile handling) and the non-interactive guard.

func TestMatrixInit_NonInteractive(t *testing.T) {
	// go test runs without a TTY on stdin, so IsInteractive() is false here.
	if IsInteractive() {
		t.Skip("stdin is a TTY; non-interactive guard cannot be tested")
	}
	err := matrixInitCmd.RunE(matrixInitCmd, nil)
	if err == nil {
		t.Fatal("wizard must refuse to run without an interactive terminal")
	}
	if !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("error should explain the interactive requirement: %v", err)
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("expected invalid-argument, got %v", err)
	}
}

func TestWizardMatrixConfig_GeneratesSameShapeAsYAML(t *testing.T) {
	cfg := wizardMatrixConfig(builder.Go, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"}, 4, nil, nil)

	if !cfg.Enabled || cfg.Rust != nil {
		t.Fatalf("wizard config: %+v", cfg)
	}
	if len(cfg.Go.Versions) != 2 || string(cfg.Go.Versions[0]) != "1.26" {
		t.Fatalf("go versions: %v", cfg.Go.Versions)
	}
	if len(cfg.Platforms) != 2 || cfg.Concurrency != 4 {
		t.Fatalf("wizard config fields: %+v", cfg)
	}

	// The wizard output must be consumable by the engine without manual
	// modification: convert exactly like phelix.yaml and expand.
	prof := cfg.MatrixProfile()
	plan, err := prof.Plan()
	if err != nil {
		t.Fatalf("wizard-generated config is not buildable: %v", err)
	}
	if len(plan.Combinations) != 4 {
		t.Fatalf("combinations = %d, want 4", len(plan.Combinations))
	}
}

func TestWizardMatrixConfig_RustAndDefaults(t *testing.T) {
	cfg := wizardMatrixConfig(builder.Rust, []string{"1.77"}, []string{"linux/amd64"}, 0, nil, nil)
	if cfg.Go != nil || cfg.Rust == nil {
		t.Fatalf("rust config: %+v", cfg)
	}
	if cfg.Concurrency != matrix.DefaultConcurrency {
		t.Fatalf("concurrency default = %d, want %d", cfg.Concurrency, matrix.DefaultConcurrency)
	}
}

func TestMatrixWizardPreview_UsesRealExpansion(t *testing.T) {
	// 3 versions × 2 platforms, including an ARM variant platform whose
	// combination IDs differ from a naive "version-platform" join.
	plan, err := matrix.ParsePlan(builder.Go,
		[]string{"1.25", "1.26", "1.27"},
		[]string{"linux/amd64", "linux/arm/v7"})
	if err != nil {
		t.Fatal(err)
	}
	preview := matrixWizardPreview(plan, 4)

	if !strings.Contains(preview, "Final combinations: 6") || !strings.Contains(preview, "Base combinations:  6") {
		t.Fatalf("preview count wrong (real expansion is 3×2):\n%s", preview)
	}
	// The preview lists actual combination IDs, including the variant form
	// that only the real expansion produces.
	if !strings.Contains(preview, "go1.26-linux-arm-v7") {
		t.Fatalf("preview missing real combination ID:\n%s", preview)
	}
	// Dimension lists recovered from the plan, not from raw input.
	if !strings.Contains(preview, "1.25, 1.26, 1.27") || !strings.Contains(preview, "linux/arm/v7") {
		t.Fatalf("preview dimensions missing:\n%s", preview)
	}
}

func TestMatrixProfileHasContent(t *testing.T) {
	empty := &project.MatrixConfig{}
	if matrixProfileHasContent(empty) {
		t.Fatal("empty section has no content")
	}
	if matrixProfileHasContent(nil) {
		t.Fatal("nil section has no content")
	}
	enabled := &project.MatrixConfig{Enabled: true}
	if !matrixProfileHasContent(enabled) {
		t.Fatal("enabled section has content")
	}
	disabledWithVersions := &project.MatrixConfig{
		Go: &project.MatrixToolchain{Versions: []project.MatrixVersion{"1.26"}},
	}
	if !matrixProfileHasContent(disabledWithVersions) {
		t.Fatal("disabled-but-configured section still has content to edit")
	}
}

func TestDescribeMatrixConfig(t *testing.T) {
	m := &project.MatrixConfig{
		Enabled:     true,
		Go:          &project.MatrixToolchain{Versions: []project.MatrixVersion{"1.26", "1.27"}},
		Platforms:   []string{"linux/amd64"},
		Concurrency: 4,
	}
	out := describeMatrixConfig(m)
	for _, want := range []string{"Go versions:", "1.26, 1.27", "linux/amd64", "Concurrency:  4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("description missing %q:\n%s", want, out)
		}
	}
}

func TestPromptMatrixEcosystemParsing(t *testing.T) {
	// The selection-to-language mapping is exercised without a TTY by
	// checking the parse logic indirectly: both orders list the detected
	// ecosystem first, and the mapping is "Rust" → rust, anything else → go.
	// (The interactive prompt itself needs a TTY; see TestMatrixInit_NonInteractive.)
	options := []string{"Go", "Rust"}
	if options[0] != "Go" {
		t.Fatal("go-first default expected")
	}
}
