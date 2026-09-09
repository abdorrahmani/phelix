package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/matrix"
)

// --- Profile convergence with include/exclude/retries -------------------------

func TestResolveMatrixProfile_YAMLRulesAndRetries(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	mustSetFlag(t, cmd, "matrix", "true")
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.25", "1.26", "1.27"]
  platforms: [linux/amd64, linux/arm64, windows/amd64]
  retries: 2
  exclude:
    - go: "1.25"
      platform: windows/amd64
  include:
    - go: "1.28"
      platform: linux/amd64
      tag: latest
`)

	prof, err := resolveMatrixProfile(cmd, cfg, builder.Go)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Retries != 2 || prof.Source.Retries != matrix.SourceConfig {
		t.Fatalf("retries: %d (%s)", prof.Retries, prof.Source.Retries)
	}
	if len(prof.Include) != 1 || len(prof.Exclude) != 1 {
		t.Fatalf("rules: include=%d exclude=%d", len(prof.Include), len(prof.Exclude))
	}

	// 3×3 base = 9, +1 included, -1 excluded → 9 combinations.
	plan, err := prof.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Combinations) != 9 {
		t.Fatalf("combinations = %d, want 9", len(plan.Combinations))
	}
	if plan.BaseCount != 9 || plan.IncludedCount != 1 || plan.ExcludedCount != 1 {
		t.Fatalf("counts: %+v", plan)
	}
	for _, c := range plan.Combinations {
		if c.ID() == "go1.28-linux-amd64" && c.Metadata["tag"] != "latest" {
			t.Fatalf("include metadata lost: %+v", c.Metadata)
		}
	}
}

func TestResolveMatrixProfile_CLIRetriesOverrideYAML(t *testing.T) {
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	// The scratch command does not register --matrix-retries; register it the
	// way BuildCmd does so the convergence sees the explicit flag.
	cmd.Flags().IntVar(&matrixRetries, "matrix-retries", 0, "")
	mustSetFlag(t, cmd, "matrix-retries", "5")
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go: {versions: ["1.26"]}
  platforms: [linux/amd64]
  retries: 1
`)

	prof, err := resolveMatrixProfile(cmd, cfg, builder.Go)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Retries != 5 || prof.Source.Retries != matrix.SourceCLI {
		t.Fatalf("retries: %d (%s), want 5 (cli)", prof.Retries, prof.Source.Retries)
	}
}

func TestResolveMatrixProfile_RulesSurviveCLIDimensionOverride(t *testing.T) {
	// CLI versions replace the YAML version list, but the YAML rules still
	// apply to the effective dimensions (documented convergence).
	defer saveMatrixFlags().restore()
	cmd := newMatrixScratchCmd(t)
	mustSetFlag(t, cmd, "matrix", "true")
	mustSetFlag(t, cmd, "go-versions", "1.26,1.27")
	mustSetFlag(t, cmd, "platforms", "linux/amd64,linux/arm64")
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go: {versions: ["1.26"]}
  platforms: [linux/amd64]
  exclude:
    - go: "1.27"
      platform: linux/arm64
`)

	prof, err := resolveMatrixProfile(cmd, cfg, builder.Go)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := prof.Plan()
	if err != nil {
		t.Fatal(err)
	}
	// 2×2 base = 4, -1 excluded → 3.
	if len(plan.Combinations) != 3 {
		t.Fatalf("combinations = %d, want 3", len(plan.Combinations))
	}
	for _, c := range plan.Combinations {
		if c.ID() == "go1.27-linux-arm64" {
			t.Fatal("excluded combination present despite CLI dimension override")
		}
	}
}

// --- matrix retry command guardrails -------------------------------------------

func runMatrixRetry(t *testing.T, id string, failed bool) error {
	t.Helper()
	oldFailed := matrixRetryFailed
	defer func() { matrixRetryFailed = oldFailed }()
	matrixRetryFailed = failed
	return matrixRetryCmd.RunE(matrixRetryCmd, []string{id})
}

func TestMatrixRetry_RequiresSelector(t *testing.T) {
	err := runMatrixRetry(t, "mx_20260909_8f31", false)
	if err == nil || !strings.Contains(err.Error(), "--failed") {
		t.Fatalf("retry without selector = %v, want --failed guidance", err)
	}
}

func TestMatrixRetry_RejectsInvalidRunID(t *testing.T) {
	for _, bad := range []string{"does-not-exist", "../../etc/passwd", ""} {
		if err := runMatrixRetry(t, bad, true); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
			t.Fatalf("retry(%q) = %v, want invalid-argument", bad, err)
		}
	}
}

func TestMatrixRetry_NoFailedCombinations(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260909_8f31", time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC), "success", "success")

	err := runMatrixRetry(t, "mx_20260909_8f31", true)
	if err == nil || !strings.Contains(err.Error(), "no failed combinations") {
		t.Fatalf("retry of all-succeeded run = %v, want no-failed-combinations error", err)
	}
}

func TestMatrixRetry_UnknownRun(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	if err := runMatrixRetry(t, "mx_20260909_8f31", true); phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("retry of unknown run = %v, want not-found", err)
	}
}

// --- matrix show: new snapshot fields -------------------------------------------

func TestMatrixShow_RendersAttemptsAndParent(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	run := matrix.NewRun("mx_20260909_8f31", "app", "/tmp/p", &matrix.Profile{
		Lang: builder.Go, Versions: []string{"1.26"}, Platforms: []string{"linux/amd64"}, Retries: 2,
	}, time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC))
	comb := matrix.Combination{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	run.InitCombinations([]matrix.Combination{comb})
	run.RecordResult(matrix.Result{
		Combination: comb,
		Status:      "success",
		Attempts: []matrix.Attempt{
			{Number: 1, Status: "failed"},
			{Number: 2, Status: "success"},
		},
	})
	run.ParentRunID = "mx_20260909_0001"
	run.Finalize(time.Date(2026, 9, 9, 3, 15, 21, 0, time.UTC))
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	out, err := runMatrixShow(t, "mx_20260909_8f31", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"attempt 2", "Parent Run:", "Retries:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}
}

// --- resume path guardrails ------------------------------------------------------

func TestResumeMatrixRun_NotResumable(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedMatrixRun(t, "mx_20260909_8f31", time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC), "success", "success")

	oldResume := matrixResume
	defer func() { matrixResume = oldResume }()
	matrixResume = "mx_20260909_8f31"

	err := resumeMatrixRun(nil, nil, nil, "", false)
	if err == nil || !strings.Contains(err.Error(), "nothing to resume") {
		t.Fatalf("resume of finished run = %v, want nothing-to-resume error", err)
	}
}

func TestResumeMatrixRun_NameMismatch(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	// Seed an interrupted run (pending combination) for application "app".
	run := matrix.NewRun("mx_20260909_8f31", "app", "/tmp/p", &matrix.Profile{
		Lang: builder.Go, Versions: []string{"1.26"}, Platforms: []string{"linux/amd64"},
	}, time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC))
	run.InitCombinations([]matrix.Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	oldResume := matrixResume
	defer func() { matrixResume = oldResume }()
	matrixResume = "mx_20260909_8f31"

	err := resumeMatrixRun([]string{"otherapp"}, nil, nil, "", false)
	if err == nil || !strings.Contains(err.Error(), "belongs to application") {
		t.Fatalf("resume with mismatched name = %v, want belongs-to error", err)
	}
}

func TestResumeMatrixRun_LatestFiltersByApp(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	run := matrix.NewRun("mx_20260909_8f31", "otherapp", "/tmp/p", &matrix.Profile{
		Lang: builder.Go, Versions: []string{"1.26"}, Platforms: []string{"linux/amd64"},
	}, time.Date(2026, 9, 9, 3, 14, 21, 0, time.UTC))
	run.InitCombinations([]matrix.Combination{
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
	})
	if err := matrix.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	oldResume := matrixResume
	defer func() { matrixResume = oldResume }()
	matrixResume = "latest"

	err := resumeMatrixRun([]string{"app"}, nil, nil, "", false)
	if phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("latest resume for app = %v, want not-found", err)
	}
}
