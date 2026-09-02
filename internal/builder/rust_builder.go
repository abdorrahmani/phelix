package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// RustBuilder builds Rust projects
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
			"cargo build failed for %s (%s)",
			config.Name,
			config.Language,
		)
	}

	// Cargo prints "Compiling <crate>" lines whenever real compilation work
	// happens; a fully fresh build only prints the "Finished" summary.
	observeCache(config, RustCacheStatusFromOutput(string(output)), buildreport.CacheSourceCargo)

	// In the actual process, after cargo build completes, we'll copy the binary
	// to the output path. This is handled in the app manager that uses this builder.
	return nil
}

// GetBinaryPath returns the path to the built binary
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

	// Try to read the package name from Cargo.toml
	pkgName := rb.getPackageNameFromCargo(config.ProjectRoot)
	if pkgName != "" {
		builtPath := filepath.Join(config.ProjectRoot, "target", buildType, pkgName)
		if _, err := os.Stat(builtPath); err == nil {
			return builtPath, nil
		}

		// Try Windows executable
		builtPathExe := builtPath + ".exe"
		if _, err := os.Stat(builtPathExe); err == nil {
			return builtPathExe, nil
		}
	}

	// Fallback: find the most recently modified binary in target/(debug|release)/
	return rb.findMostRecentBinary(config.ProjectRoot, buildType)
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

// findMostRecentBinary finds the most recently modified binary in the target directory
func (rb *RustBuilder) findMostRecentBinary(projectRoot string, buildType string) (string, error) {
	targetDir := filepath.Join(projectRoot, "target", buildType)
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read target directory")
	}

	var newest os.DirEntry
	var newestTime int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Unix() > newestTime {
			newest = entry
			newestTime = info.ModTime().Unix()
		}
	}

	if newest == nil {
		return "", phelixerr.Newf(phelixerr.CodeNotFound, "no binary found in target/%s", buildType)
	}

	return filepath.Join(targetDir, newest.Name()), nil
}

// CopyBuiltBinary copies the built rust binary to the specified output path
func CopyBuiltBinary(sourcePath string, outputPath string) error {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read built binary")
	}

	if err := os.WriteFile(outputPath, data, 0755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write output binary")
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

	// Get binary path
	binaryPath, err := rb.GetBinaryPath(config)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeNotFound, err, "failed to locate built binary")
	}

	// Copy to output path
	if err := CopyBuiltBinary(binaryPath, config.OutputPath); err != nil {
		return err
	}

	return nil
}
