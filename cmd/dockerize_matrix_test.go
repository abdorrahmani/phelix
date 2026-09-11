package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/docker"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/spf13/cobra"
)

func TestParseDockerBuildArgs(t *testing.T) {
	got, err := parseDockerBuildArgs([]string{"VERSION=1.2.3", "EMPTY="})
	if err != nil {
		t.Fatalf("parseDockerBuildArgs: %v", err)
	}
	if got["VERSION"] != "1.2.3" || got["EMPTY"] != "" {
		t.Fatalf("unexpected args: %#v", got)
	}
}

func TestParseDockerBuildArgsRejectsInvalid(t *testing.T) {
	for _, value := range []string{"VERSION", "=value"} {
		if _, err := parseDockerBuildArgs([]string{value}); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}

func TestMatrixManifestTag(t *testing.T) {
	tests := []struct {
		name, base, version, want string
		count                     int
	}{
		{name: "single defaults latest", version: "1.23", count: 1, want: "latest"},
		{name: "single uses base", base: "v2", version: "1.23", count: 1, want: "v2"},
		{name: "multiple qualify base", base: "v2", version: "1.23", count: 2, want: "v2-1.23"},
		{name: "multiple qualify latest", version: "go1.23", count: 2, want: "latest-1.23"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matrixManifestTag(tt.base, tt.version, tt.count); got != tt.want {
				t.Fatalf("matrixManifestTag() = %q, want %q", got, tt.want)
			}
		})
	}
}

// dockerizeMatrixFlagState snapshots the dockerize matrix flag variables so
// tests can manipulate them through a scratch command and restore the world.
type dockerizeMatrixFlagState struct {
	matrix       bool
	goVersions   []string
	rustVersions []string
	platforms    []string
	concurrency  int
	retries      int
	dryRun       bool
	multiArchTag bool
	push         bool
}

func saveDockerizeMatrixFlags() dockerizeMatrixFlagState {
	return dockerizeMatrixFlagState{
		matrix: dockerizeMatrix, goVersions: dockerizeGoVersions, rustVersions: dockerizeRustVersions,
		platforms: dockerizePlatforms, concurrency: dockerizeConcurrency, retries: dockerizeMatrixRetries,
		dryRun: dockerizeMatrixDryRun, multiArchTag: dockerizeMultiArchTag, push: dockerizePush,
	}
}

func (s dockerizeMatrixFlagState) restore() {
	dockerizeMatrix = s.matrix
	dockerizeGoVersions = s.goVersions
	dockerizeRustVersions = s.rustVersions
	dockerizePlatforms = s.platforms
	dockerizeConcurrency = s.concurrency
	dockerizeMatrixRetries = s.retries
	dockerizeMatrixDryRun = s.dryRun
	dockerizeMultiArchTag = s.multiArchTag
	dockerizePush = s.push
}

// newDockerizeMatrixScratchCmd builds a command with the same matrix flags as
// dockerize, bound to the same package variables.
func newDockerizeMatrixScratchCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "dscratch"}
	cmd.Flags().BoolVar(&dockerizeMatrix, "matrix", false, "")
	cmd.Flags().StringSliceVar(&dockerizeGoVersions, "go-versions", nil, "")
	cmd.Flags().StringSliceVar(&dockerizeRustVersions, "rust-versions", nil, "")
	cmd.Flags().StringSliceVar(&dockerizePlatforms, "platforms", nil, "")
	cmd.Flags().IntVar(&dockerizeConcurrency, "matrix-concurrency", matrix.DefaultConcurrency, "")
	cmd.Flags().IntVar(&dockerizeMatrixRetries, "matrix-retries", 0, "")
	cmd.Flags().BoolVar(&dockerizeMatrixDryRun, "matrix-dry-run", false, "")
	return cmd
}

// `phelix dockerize` must activate and resolve the matrix exactly like
// `phelix build`: an enabled phelix.yaml profile activates it with no flags,
// CLI dimensions replace (never merge) the profile's lists, and an explicit
// --matrix=false disables the profile.
func TestDockerizeMatrix_YAMLProfileActivatesAndResolves(t *testing.T) {
	defer saveDockerizeMatrixFlags().restore()
	cmd := newDockerizeMatrixScratchCmd(t)
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26", "1.27"]
  platforms: [linux/amd64, linux/arm64]
  concurrency: 4
  retries: 2
`)
	in := dockerizeMatrixResolveInput(cmd, cfg, builder.Go)
	if !matrix.IsActive(in) {
		t.Fatal("enabled phelix.yaml profile must activate the dockerize matrix without --matrix")
	}
	prof, active, err := matrix.Resolve(in)
	if err != nil || !active {
		t.Fatalf("resolve: active=%v err=%v", active, err)
	}
	if len(prof.Versions) != 2 || prof.Concurrency != 4 || prof.Retries != 2 {
		t.Fatalf("profile must carry the YAML dimensions: %+v", prof)
	}
}

func TestDockerizeMatrix_ExplicitFalseDisablesProfile(t *testing.T) {
	defer saveDockerizeMatrixFlags().restore()
	cmd := newDockerizeMatrixScratchCmd(t)
	mustSetFlag(t, cmd, "matrix", "false")
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
`)
	in := dockerizeMatrixResolveInput(cmd, cfg, builder.Go)
	if matrix.IsActive(in) {
		t.Fatal("--matrix=false must disable an enabled profile for dockerize too")
	}
	if _, active, err := matrix.Resolve(in); err != nil || active {
		t.Fatalf("resolve: active=%v err=%v", active, err)
	}
}

func TestDockerizeMatrix_CLIVersionsReplaceYAML(t *testing.T) {
	defer saveDockerizeMatrixFlags().restore()
	cmd := newDockerizeMatrixScratchCmd(t)
	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26", "1.27"]
  platforms: [linux/amd64, linux/arm64]
`)
	mustSetFlag(t, cmd, "go-versions", "1.28")

	in := dockerizeMatrixResolveInput(cmd, cfg, builder.Go)
	prof, _, err := matrix.Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(prof.Versions) != 1 || prof.Versions[0] != "1.28" {
		t.Fatalf("CLI versions must replace the YAML list, got %v", prof.Versions)
	}
	if len(prof.Platforms) != 2 {
		t.Fatalf("YAML platforms must survive, got %v", prof.Platforms)
	}
}

func TestDockerizeMatrix_MatrixFalseWithDimensionsRejected(t *testing.T) {
	defer saveDockerizeMatrixFlags().restore()
	cmd := newDockerizeMatrixScratchCmd(t)
	mustSetFlag(t, cmd, "matrix", "false")
	mustSetFlag(t, cmd, "go-versions", "1.28")

	cfg := loadMatrixYAMLCfg(t, `
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
`)
	in := dockerizeMatrixResolveInput(cmd, cfg, builder.Go)
	if _, _, err := matrix.Resolve(in); err == nil {
		t.Fatal("--matrix=false + --go-versions must be rejected for dockerize too")
	}
}

func TestDockerizeMatrix_FlagSurface(t *testing.T) {
	// The dockerize matrix flags documented in the README exist and bind.
	for _, name := range []string{"matrix", "go-versions", "rust-versions", "platforms",
		"matrix-tags", "multi-arch-tag", "push-partial", "matrix-concurrency",
		"matrix-retries", "matrix-dry-run", "debug"} {
		if DockerizeCmd.Flags().Lookup(name) == nil {
			t.Fatalf("dockerize flag --%s missing", name)
		}
	}
}

// --multi-arch-tag assembles manifest lists from pushed registry images
// (docker buildx imagetools create works on registry references), so it
// requires --push. The check fires before any build work starts.
func TestRunDockerizeMatrix_MultiArchRequiresPush(t *testing.T) {
	defer saveDockerizeMatrixFlags().restore()
	dockerizeMultiArchTag = true
	dockerizePush = false
	in := matrix.ResolveInput{
		DetectedLang: builder.Go, MatrixFlag: true,
		CLI: matrix.CLIOptions{GoVersions: []string{"1.26"}, Platforms: []string{"linux/amd64"}},
	}
	err := runDockerizeMatrixMode("app", builder.Go, t.TempDir(), in)
	if err == nil {
		t.Fatal("--multi-arch-tag without --push must be rejected")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument || !strings.Contains(err.Error(), "--push") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --matrix-dry-run prints the plan and exits before any Docker interaction:
// no Dockerfile is generated, nothing is built.
func TestRunDockerizeMatrix_DryRunDoesNotTouchTheProject(t *testing.T) {
	defer saveDockerizeMatrixFlags().restore()
	dockerizeMatrixDryRun = true
	dir := t.TempDir()
	in := matrix.ResolveInput{
		DetectedLang: builder.Go, MatrixFlag: true,
		CLI: matrix.CLIOptions{GoVersions: []string{"1.26", "1.27"}, Platforms: []string{"linux/amd64", "linux/arm64"}},
	}
	if err := runDockerizeMatrixMode("app", builder.Go, dir, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil {
		t.Fatal("dry run must not generate a Dockerfile")
	}
	if _, err := os.Stat(filepath.Join(dir, "builds", "matrix", "report.json")); err == nil {
		t.Fatal("dry run must not write a report")
	}
}

// The matrix dockerize path must ensure a Dockerfile exists exactly like the
// single-image path: generated when absent, existing files kept as-is.
func TestEnsureDockerfileAndIgnore(t *testing.T) {
	dir := t.TempDir()
	if err := ensureDockerfileAndIgnore(dir, docker.LanguageGo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatalf("Dockerfile not generated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".dockerignore")); err != nil {
		t.Fatalf(".dockerignore not generated: %v", err)
	}

	// An existing Dockerfile is used as-is, never overwritten.
	existing := []byte("# custom Dockerfile\n")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), existing, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureDockerfileAndIgnore(dir, docker.LanguageGo); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil || string(got) != string(existing) {
		t.Fatalf("existing Dockerfile must be preserved, got %q (%v)", got, err)
	}
}
