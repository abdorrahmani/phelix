package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/spf13/cobra"
)

// matrixFlagState snapshots the package-level matrix flag variables so tests
// can manipulate them through a scratch command and restore the world after.
type matrixFlagState struct {
	matrixFlag        bool
	goVersions        []string
	rustVersions      []string
	platforms         []string
	matrixConcurrency int
}

func saveMatrixFlags() matrixFlagState {
	return matrixFlagState{
		matrixFlag: matrixFlag, goVersions: goVersions, rustVersions: rustVersions,
		platforms: platforms, matrixConcurrency: matrixConcurrency,
	}
}

func (s matrixFlagState) restore() {
	matrixFlag = s.matrixFlag
	goVersions = s.goVersions
	rustVersions = s.rustVersions
	platforms = s.platforms
	matrixConcurrency = s.matrixConcurrency
}

// newMatrixScratchCmd builds a command with the same matrix flags as build,
// bound to the same package variables, so resolution sees exactly what the
// real CLI would see.
func newMatrixScratchCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "scratch"}
	cmd.Flags().BoolVar(&matrixFlag, "matrix", false, "")
	cmd.Flags().StringSliceVar(&goVersions, "go-versions", nil, "")
	cmd.Flags().StringSliceVar(&rustVersions, "rust-versions", nil, "")
	cmd.Flags().StringSliceVar(&platforms, "platforms", nil, "")
	cmd.Flags().IntVar(&matrixConcurrency, "matrix-concurrency", matrix.DefaultConcurrency, "")
	return cmd
}

func mustSetFlag(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	if err := cmd.Flags().Set(name, value); err != nil {
		t.Fatal(err)
	}
}

func loadMatrixYAMLCfg(t *testing.T, yml string) *project.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := project.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// --- Resolution: CLI/YAML convergence at the cmd layer --------------------------

func TestMatrixActive_YAMLProfileWithoutFlag(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
`)
	if !matrixActive(cmd, cfg) {
		t.Fatal("enabled phelix.yaml profile must activate the matrix without --matrix")
	}
	if matrixActive(cmd, nil) {
		t.Fatal("no flags, no profile → inactive")
	}
}

func TestResolveMatrixProfile_BackwardCompatibleCLIOnly(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	mustSetFlag(t, cmd, "matrix", "true")
	mustSetFlag(t, cmd, "go-versions", "1.26,1.27")
	mustSetFlag(t, cmd, "platforms", "linux/amd64,linux/arm64")

	prof, err := resolveMatrixProfile(cmd, nil, builder.Go)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := prof.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Combinations) != 4 {
		t.Fatalf("combinations = %d, want 4 (legacy CLI matrix behavior)", len(plan.Combinations))
	}
	if prof.Source.Versions != matrix.SourceCLI || prof.Source.Platforms != matrix.SourceCLI {
		t.Fatalf("sources: %+v", prof.Source)
	}
}

func TestResolveMatrixProfile_CLIOverridesYAML(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26", "1.27"]
  platforms: [linux/amd64, linux/arm64]
  concurrency: 4
`)
	// CLI overrides versions only; platforms and concurrency stay from YAML.
	mustSetFlag(t, cmd, "go-versions", "1.28")

	prof, err := resolveMatrixProfile(cmd, cfg, builder.Go)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(prof.Versions, ",") != "1.28" {
		t.Fatalf("versions = %v, want CLI-only [1.28] (replace, never merge)", prof.Versions)
	}
	if len(prof.Platforms) != 2 {
		t.Fatalf("platforms = %v, want YAML values", prof.Platforms)
	}
	if prof.Concurrency != 4 {
		t.Fatalf("concurrency = %d, want YAML 4", prof.Concurrency)
	}
}

func TestResolveMatrixProfile_CLIConcurrencyBeatsYAML(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
  concurrency: 4
`)
	mustSetFlag(t, cmd, "matrix-concurrency", "8")

	prof, err := resolveMatrixProfile(cmd, cfg, builder.Go)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Concurrency != 8 {
		t.Fatalf("concurrency = %d, want explicit CLI 8", prof.Concurrency)
	}
}

func TestResolveMatrixProfile_DefaultConcurrencyFromYAMLOnly(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
  concurrency: 6
`)
	// --matrix-concurrency not set explicitly: its flag default (3) must NOT
	// shadow the YAML value.
	prof, err := resolveMatrixProfile(cmd, cfg, builder.Go)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Concurrency != 6 {
		t.Fatalf("concurrency = %d, want YAML 6 (unset flag default must not win)", prof.Concurrency)
	}
}

func TestResolveMatrixProfile_MissingVersions(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	mustSetFlag(t, cmd, "matrix", "true")
	// Neither flags nor YAML supply versions.
	_, err := resolveMatrixProfile(cmd, nil, builder.Go)
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("expected invalid-argument, got %v", err)
	}
}

func TestResolveMatrixProfile_LanguageMismatch(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	mustSetFlag(t, cmd, "rust-versions", "1.77")
	mustSetFlag(t, cmd, "platforms", "linux/amd64")

	_, err := resolveMatrixProfile(cmd, nil, builder.Go)
	if err == nil || !strings.Contains(err.Error(), "detected project language is go") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
}

// --- phelix matrix list ------------------------------------------------------------

func seedMatrixRun(t *testing.T, id string, started time.Time, statuses ...string) {
	t.Helper()
	run := matrix.NewRun(matrix.RunID(id), "app", "/tmp/p", &matrix.Profile{
		Lang: builder.Go, Versions: []string{"1.26"}, Platforms: []string{"linux/amd64"}, Concurrency: 2,
	}, started)
	results := make([]matrix.Result, 0, len(statuses))
	for _, s := range statuses {
		results = append(results, matrix.Result{
			Combination: matrix.Combination{
				Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64",
			},
			Status: s,
		})
	}
	run.Finish(results, started.Add(time.Minute))
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}
}

// runMatrixList invokes the list command with the given flag values,
// capturing stdout. Flag globals are restored afterwards.
func runMatrixList(t *testing.T, jsonOut bool, limit int) (string, error) {
	t.Helper()
	oldLimit, oldJSON := matrixListLimit, matrixListJSON
	defer func() { matrixListLimit, matrixListJSON = oldLimit, oldJSON }()
	matrixListLimit, matrixListJSON = limit, jsonOut

	var err error
	out := captureStdout(t, func() {
		err = matrixListCmd.RunE(matrixListCmd, nil)
	})
	return out, err
}

func TestMatrixList_Empty(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	out, err := runMatrixList(t, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No matrix runs") {
		t.Fatalf("empty-list output: %q", out)
	}
}

func TestMatrixList_ShowsRunsNewestFirst(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260908_a12f", time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC), "failed", "failed")
	seedMatrixRun(t, "mx_20260909_8f31", time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC), "success", "success")

	out, err := runMatrixList(t, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	idxNew := strings.Index(out, "mx_20260909_8f31")
	idxOld := strings.Index(out, "mx_20260908_a12f")
	if idxNew == -1 || idxOld == -1 {
		t.Fatalf("runs missing from output: %q", out)
	}
	if idxNew > idxOld {
		t.Fatalf("newest run must be listed first: %q", out)
	}
	if !strings.Contains(out, "succeeded") || !strings.Contains(out, "failed") {
		t.Fatalf("statuses missing from output: %q", out)
	}
}

func TestMatrixList_JSON(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260909_8f31", time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC), "success", "success")

	out, err := runMatrixList(t, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		t.Fatalf("JSON output is not a valid array: %v\n%s", err, out)
	}
	if len(runs) != 1 || runs[0]["id"] != "mx_20260909_8f31" {
		t.Fatalf("JSON runs: %v", runs)
	}
	if runs[0]["status"] != string(matrix.RunStatusSucceeded) || runs[0]["total"] != float64(2) {
		t.Fatalf("JSON fields: %v", runs[0])
	}
}

func TestMatrixList_Limit(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260909_0001", time.Date(2026, 9, 9, 1, 0, 0, 0, time.UTC), "success")
	seedMatrixRun(t, "mx_20260909_0002", time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC), "success")
	seedMatrixRun(t, "mx_20260909_0003", time.Date(2026, 9, 9, 3, 0, 0, 0, time.UTC), "success")

	out, err := runMatrixList(t, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mx_20260909_0003") {
		t.Fatalf("newest run missing: %q", out)
	}
	if strings.Contains(out, "mx_20260909_0001") || strings.Contains(out, "mx_20260909_0002") {
		t.Fatalf("limit not applied: %q", out)
	}
}

// --- phelix matrix show ------------------------------------------------------------

func runMatrixShow(t *testing.T, id string, jsonOut bool) (string, error) {
	t.Helper()
	oldJSON := matrixShowJSON
	defer func() { matrixShowJSON = oldJSON }()
	matrixShowJSON = jsonOut

	var err error
	out := captureStdout(t, func() {
		err = matrixShowCmd.RunE(matrixShowCmd, []string{id})
	})
	return out, err
}

func TestMatrixShow_ValidRun(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260909_8f31", time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC), "success")

	out, err := runMatrixShow(t, "mx_20260909_8f31", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Matrix Run: mx_20260909_8f31",
		"Configuration",
		"linux/amd64",
		"go1.26-linux-amd64",
		"Total:    1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}
}

func TestMatrixShow_UnknownRun(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	_, err := runMatrixShow(t, "mx_20260909_dead", false)
	if phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("expected CodeNotFound for unknown run, got %v", err)
	}
}

func TestMatrixShow_InvalidID(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	for _, bad := range []string{"does-not-exist", "../../etc/passwd", ""} {
		_, err := runMatrixShow(t, bad, false)
		if phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
			t.Fatalf("show(%q) = %v, want invalid-argument", bad, err)
		}
	}
}

func TestMatrixShow_JSON(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260909_8f31", time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC), "success", "failed")

	out, err := runMatrixShow(t, "mx_20260909_8f31", true)
	if err != nil {
		t.Fatal(err)
	}
	var run matrix.Run
	if err := json.Unmarshal([]byte(out), &run); err != nil {
		t.Fatalf("JSON output invalid: %v\n%s", err, out)
	}
	if run.ID != "mx_20260909_8f31" || run.Status != matrix.RunStatusPartial {
		t.Fatalf("run: %+v", &run)
	}
	if len(run.Combinations) != 2 || run.Combinations[0].Identity != "mx_20260909_8f31/go1.26-linux-amd64" {
		t.Fatalf("combinations: %+v", run.Combinations)
	}
}

func TestMatrixShow_RequiresExactlyOneArg(t *testing.T) {
	if err := matrixShowCmd.Args(matrixShowCmd, nil); err == nil {
		t.Fatal("show requires an argument")
	}
	if err := matrixShowCmd.Args(matrixShowCmd, []string{"mx_20260909_8f31"}); err != nil {
		t.Fatalf("one arg must be accepted: %v", err)
	}
}

// --- Flag validation --------------------------------------------------------------

func TestMatrixFlagConflict(t *testing.T) {
	cases := []struct {
		name                        string
		set, flag                   bool
		goVers, rustVers, platforms []string
		wantErr                     bool
	}{
		{"false plus go versions", true, false, []string{"1.26"}, nil, nil, true},
		{"false plus rust versions", true, false, nil, []string{"1.77"}, nil, true},
		{"false plus platforms", true, false, nil, nil, []string{"linux/amd64"}, true},
		{"true plus go versions", true, true, []string{"1.26"}, nil, nil, false},
		{"false alone", true, false, nil, nil, nil, false},
		{"unset plus go versions", false, false, []string{"1.26"}, nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := matrixFlagConflict(tc.set, tc.flag, tc.goVers, tc.rustVers, tc.platforms)
			if (err != nil) != tc.wantErr {
				t.Fatalf("matrixFlagConflict = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
				t.Fatalf("conflict error code = %s", phelixerr.CodeOf(err))
			}
		})
	}
}

func TestValidateMatrixFlagValues(t *testing.T) {
	cases := []struct {
		name           string
		concurrency    int
		concurrencySet bool
		retries        int
		retriesSet     bool
		wantErr        bool
	}{
		{"explicit zero concurrency", 0, true, 0, false, true},
		{"explicit negative concurrency", -2, true, 0, false, true},
		{"explicit valid concurrency", 1, true, 0, false, false},
		{"unset default concurrency", matrix.DefaultConcurrency, false, 0, false, false},
		{"explicit negative retries", 3, true, -1, true, true},
		{"explicit zero retries", 3, true, 0, true, false},
		{"unset retries", 3, true, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMatrixFlagValues(tc.concurrency, tc.concurrencySet, tc.retries, tc.retriesSet)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateMatrixFlagValues = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// --- JSON purity -------------------------------------------------------------------

// A malformed run file must not break `matrix list --json`: the warning is
// terminal-only, so the JSON stream stays parseable.
func TestMatrixList_JSONWithMalformedRuns(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260909_8f31", time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC), "success", "success")
	dir, err := matrix.RunsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mx_20260909_bad1.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runMatrixList(t, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		t.Fatalf("JSON output must stay valid despite malformed run files: %v\n%s", err, out)
	}
	if len(runs) != 1 || runs[0]["id"] != "mx_20260909_8f31" {
		t.Fatalf("JSON runs: %v", runs)
	}
}

// --- Include-rule metadata in matrix show -------------------------------------------

func TestMatrixShow_RendersMetadata(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	run := matrix.NewRun("mx_20260909_9e7a", "app", "/tmp/p", &matrix.Profile{
		Lang: builder.Go, Versions: []string{"1.26"}, Platforms: []string{"linux/amd64"}, Concurrency: 2,
	}, time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC))
	run.InitCombinations([]matrix.Combination{
		{
			Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64",
			Metadata: map[string]string{"tag": "latest", "channel": "stable"},
		},
	})
	run.RecordResult(matrix.Result{
		Combination: matrix.Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		Status:      "success", Artifact: "/tmp/bin",
	})
	run.Finalize(time.Date(2026, 9, 9, 3, 15, 0, 0, time.UTC))
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	out, err := runMatrixShow(t, "mx_20260909_9e7a", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Metadata:", "tag=latest", "channel=stable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}
}
