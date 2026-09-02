package builder

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// Validate checks if a valid Rust project exists at the given root
func (rb *RustBuilder) Validate(projectRoot string) error {
	if _, err := os.Stat(projectRoot); os.IsNotExist(err) {
		return phelixerr.Newf(phelixerr.CodeNotFound, "project root does not exist: %s", projectRoot)
	}

	cargoPath := filepath.Join(projectRoot, "Cargo.toml")
	if _, err := os.Stat(cargoPath); os.IsNotExist(err) {
		return phelixerr.New(phelixerr.CodeUnsupportedProject, "no Cargo.toml found in project root")
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

	cmd := exec.Command("cargo", args...)
	cmd.Dir = config.ProjectRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Compiler output is not dumped into the message. It is kept as a
		// bounded tail on the ToolError for the CLI error reporter; the
		// underlying *exec.ExitError stays reachable through the wraps.
		return phelixerr.Wrapf(
			phelixerr.CodeBuildFailed,
			&ToolError{Tool: "cargo", Output: tailOutput(output), Err: err},
			"%s",
			cargoFailureMessage(err, output, config),
		)
	}

	// Cargo prints "Compiling <crate>" lines whenever real compilation work
	// happens; a fully fresh build only prints the "Finished" summary.
	observeCache(config, RustCacheStatusFromOutput(string(output)), buildreport.CacheSourceCargo)

	// In the actual process, after cargo build completes, we'll copy the binary
	// to the output path. This is handled in the app manager that uses this builder.
	return nil
}

// cargoFailureMessage renders the human-facing reason a cargo invocation
// failed: the exit status plus the last few lines of its diagnostics (the
// error itself is usually at the end of the output). Bounded so a runaway
// build log cannot flood the terminal; redacted because the output is not
// Phelix-authored.
func cargoFailureMessage(err error, output []byte, config BuildConfig) string {
	status := "unknown"
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		status = fmt.Sprintf("exit status %d", exitErr.ExitCode())
	} else if !errors.Is(err, exec.ErrNotFound) && err != nil {
		// cargo itself could not be started (missing toolchain, etc.)
		return fmt.Sprintf("cargo build failed for %s: %v", config.Name, err)
	}

	msg := fmt.Sprintf("cargo build failed for %s (%s): %s", config.Name, config.Language, status)
	if tail := cargoOutputTail(output); tail != "" {
		msg += "\n" + tail
	}
	return msg
}

// cargoOutputTail returns the last few non-empty lines of cargo's combined
// output, redacted and capped, for inclusion in the build error message.
func cargoOutputTail(output []byte) string {
	const maxLines = 8
	const maxBytes = 1024

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var keep []string
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			keep = append(keep, line)
		}
	}
	if len(keep) > maxLines {
		keep = append([]string{fmt.Sprintf("… (%d more lines)", len(keep)-maxLines)}, keep[len(keep)-maxLines:]...)
	}
	joined := strings.Join(keep, "\n")
	if len(joined) > maxBytes {
		joined = joined[len(joined)-maxBytes:]
		// Don't cut mid-line.
		if idx := strings.IndexByte(joined, '\n'); idx >= 0 {
			joined = joined[idx+1:]
		}
	}
	return phelixerr.Redact(joined)
}

// GetBinaryPath returns the path to the built binary.
//
// Resolution order: the exact package-name binary, then an exact `[[bin]]`
// name from Cargo.toml, then the most recently modified executable file in
// target/<buildType>. When none exists, the returned error names the expected
// location and why — a cargo success with no artifact is always an explicit,
// actionable failure, never a silent fallback to whatever file happens to be
// newest (cargo also leaves .d/.rlib/.a files there, which the old fallback
// happily mistook for the binary).
func (rb *RustBuilder) GetBinaryPath(config BuildConfig) (string, error) {
	// Determine if it's a release build
	isRelease := false
	for _, arg := range config.ExtraArgs {
		if arg == "--release" {
			isRelease = true
			break
		}
	}

	buildType := "debug"
	if isRelease {
		buildType = "release"
	}
	targetDir := filepath.Join(config.ProjectRoot, "target", buildType)

	pkgName := rb.getPackageNameFromCargo(config.ProjectRoot)
	candidates := []string{}
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
	for _, line := range strings.Split(string(data), "\n") {
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

	for _, line := range strings.Split(string(data), "\n") {
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
