package builder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
) // RustBuilder builds Rust projects

type RustBuilder struct{}

// NewRustBuilder creates a new Rust builder
func NewRustBuilder() *RustBuilder {
	return &RustBuilder{}
}

// Name returns "rust"
func (rb *RustBuilder) Name() Language {
	return Rust
}

// Validate checks that scan describes a buildable Rust project.
func (rb *RustBuilder) Validate(scan *ProjectScan) error {
	if scan == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "validate : no project scan")
	}

	if !scan.Exists {
		return phelixerr.Newf(phelixerr.CodeNotFound, "project root does not exist: %s", scan.Root)
	}

	if !scan.RootFile("Cargo.toml") {
		return phelixerr.New(phelixerr.CodeUnsupportedProject, "no valid Rust project found: missing Cargo.toml")
	}

	return nil
}

// Build performs a Rust build
func (rb *RustBuilder) Build(config BuildConfig) error {
	// Build command arguments
	args := []string{"build"}

	// Add extra arguments from user (e.g., --release, --features, etc.)
	if len(config.ExtraArgs) > 0 {
		args = append(args, config.ExtraArgs...)
	}

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout(config.Timeout))
	defer cancel()

	cmd := exec.CommandContext(ctx, "cargo", args...)
	cmd.Dir = config.ProjectRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return phelixerr.Wrapf(phelixerr.CodeBuildTimeout, err,
				"cargo build for %s exceeded the %s build timeout; raise it with --build-timeout",
				config.Name, buildTimeout(config.Timeout))
		}

		return phelixerr.Wrapf(
			phelixerr.CodeBuildFailed,
			&ToolError{Tool: "cargo", Output: tailOutput(output), Err: err},
			"%s",
			cargoFailureMessage(err, config),
		)
	}

	observeCache(config, RustCacheStatusFromOutput(string(output)), buildreport.CacheSourceCargo)

	return nil
}

// cargoFailureMessage renders the human-facing reason a cargo invocation
// failed. It names the build and the exit status; the tool's diagnostic tail
// lives only on the ToolError (see Build) so raw compiler output is never
// echoed into error messages unredacted.
func cargoFailureMessage(err error, config BuildConfig) string {
	if !errors.Is(err, exec.ErrNotFound) {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) && err != nil {
			// cargo itself could not be started (missing toolchain, etc.)
			return fmt.Sprintf("cargo build failed for %s: %v", config.Name, err)
		}
		return fmt.Sprintf("cargo build failed for %s (%s): exit status %d",
			config.Name, config.Language, exitErr.ExitCode())
	}
	return fmt.Sprintf("cargo build failed for %s: %v", config.Name, err)
}

// GetBinaryPath returns the path to the built binary.
func (rb *RustBuilder) GetBinaryPath(config BuildConfig) (string, error) {
	// Determine if it's a release build
	isRelease := slices.Contains(config.ExtraArgs, "--release")

	buildType := "debug"
	if isRelease {
		buildType = "release"
	}
	targetDir := filepath.Join(config.ProjectRoot, "target", buildType)

	pkgName := rb.getPackageNameFromCargo(config.ProjectRoot)
	var candidates []string
	if pkgName != "" {
		candidates = append(candidates, pkgName)
	}
	candidates = append(candidates, binNamesFromCargo(config.ProjectRoot)...)

	for _, name := range candidates {
		builtPath := filepath.Join(targetDir, name)
		if _, err := os.Stat(builtPath); err == nil {
			return builtPath, nil
		}
		// Try Windows executable
		if _, err := os.Stat(builtPath + ".exe"); err == nil {
			return builtPath + ".exe", nil
		}
	}

	path, err := rb.findMostRecentBinary(targetDir)
	if err != nil {
		return "", rb.missingArtifactError(config, targetDir, pkgName, buildType, err)
	}
	return path, nil
}

// missingArtifactError builds the actionable error for "cargo succeeded but
// the artifact is not where we expect it". It names the expected path, the
// detected package, and whether the cache was involved, so a wrong binary
// name / workspace layout / stale target dir is diagnosable from the message.
func (rb *RustBuilder) missingArtifactError(config BuildConfig, targetDir, pkgName, buildType string, cause error) error {
	cache := "unknown"
	if config.Observe != nil {
		cache = string(config.Observe.CacheStatus.Normalized())
	}
	name := pkgName
	if name == "" {
		name = "(unknown — no name in Cargo.toml)"
	}
	msg := fmt.Sprintf(
		"cargo reported success but no binary was produced: expected an executable in %s (package %q, build type %s, cache %s)",
		targetDir, name, buildType, cache,
	)
	if cause != nil {
		return phelixerr.Wrapf(phelixerr.CodeNotFound, cause, "%s — check the crate's [[bin]] name or clear target/ if the cache is stale", msg)
	}
	return phelixerr.Newf(phelixerr.CodeNotFound, "%s — check the crate's [[bin]] name or clear target/ if the cache is stale", msg)
}

// binNamesFromCargo extracts the `name = "..."` entries of every `[[bin]]`
// section in Cargo.toml, so crates whose binary differs from the package name
// still resolve.
func binNamesFromCargo(projectRoot string) []string {
	cargoPath := filepath.Join(projectRoot, "Cargo.toml")
	data, err := os.ReadFile(cargoPath)
	if err != nil {
		return nil
	}

	var names []string
	inBin := false
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inBin = trimmed == "[[bin]]"
			continue
		}
		if inBin && strings.HasPrefix(trimmed, "name") && strings.Contains(trimmed, "=") {
			parts := strings.SplitN(trimmed, "=", 2)
			names = append(names, strings.Trim(strings.TrimSpace(parts[1]), "\"'"))
		}
	}
	return names
}

// GetBuildFlags returns Rust/Cargo build flags
func (rb *RustBuilder) GetBuildFlags() []string {
	return []string{
		"--release",
		"--debug",
		"--features",
		"--no-default-features",
		"--all-features",
		"--target",
		"--all-targets",
		"--lib",
		"--bins",
		"--bin",
		"--example",
		"--examples",
		"--test",
		"--tests",
		"--bench",
		"--benches",
		"--all",
		"--workspace",
		"--exclude",
		"-p",
		"--package",
		"--jobs",
		"-j",
		"--timings",
		"--incremental",
		"--no-incremental",
		"--build-plan",
	}
}

// getPackageNameFromCargo reads the package name from Cargo.toml
func (rb *RustBuilder) getPackageNameFromCargo(projectRoot string) string {
	cargoPath := filepath.Join(projectRoot, "Cargo.toml")
	data, err := os.ReadFile(cargoPath)
	if err != nil {
		return ""
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name") && strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				pkgName := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
				return pkgName
			}
		}
	}
	return ""
}

// findMostRecentBinary finds the most recently modified executable file in
// the target directory. Library/dependency artifacts (`.d`, `.rlib`, `.a`,
// `.so`) are ignored — they share the directory with the binary and are newer
// than it whenever cargo relinks a dependency.
func (rb *RustBuilder) findMostRecentBinary(targetDir string) (string, error) {
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return "", err
	}

	var newest string
	var newestTime time.Time
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Executable-bit check: a "binary" cargo emits is always +x.
		if info.Mode().Perm()&0o111 == 0 {
			continue
		}

		if newest == "" || info.ModTime().After(newestTime) {
			newest = entry.Name()
			newestTime = info.ModTime()
		}
	}

	if newest == "" {
		return "", phelixerr.Newf(phelixerr.CodeNotFound, "no executable found in %s", targetDir)
	}

	return filepath.Join(targetDir, newest), nil
}

// CopyBuiltBinary copies the built rust binary to the specified output path.
//
// The write goes to a sibling temp file that is then renamed over the target:
// the output path (<source-dir>/app_<id>) may itself be the executable a
// running instance was started from, and opening it for writing fails with
// ETXTBSY ("text file busy"). Rename swaps the directory entry atomically, so
// the running process keeps executing its own inode while the path comes to
// point at the new build — no stop-before-build, no downtime.
func CopyBuiltBinary(sourcePath string, outputPath string) error {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read built binary")
	}

	tmp := outputPath + fmt.Sprintf(".phelix-tmp-%d", os.Getpid())
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write output binary")
	}
	if err := os.Rename(tmp, outputPath); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to replace output binary")
	}

	return nil
}

// BuildAndCopyRust is a convenience function that builds and copies the binary
func BuildAndCopyRust(config BuildConfig) error {
	rb := NewRustBuilder()

	// Build
	if err := rb.Build(config); err != nil {
		return err
	}

	// Get binary path (detailed, actionable error when the artifact is missing)
	binaryPath, err := rb.GetBinaryPath(config)
	if err != nil {
		return err
	}

	// Copy to output path
	if err := CopyBuiltBinary(binaryPath, config.OutputPath); err != nil {
		return err
	}

	return nil
}
