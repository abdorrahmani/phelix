package builder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/cargo"
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

// Build performs a Rust build and records the exact executable cargo reported
// for the selected package and binary target.
func (rb *RustBuilder) Build(config BuildConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout(config.Timeout))
	defer cancel()

	selection, err := cargo.SelectFor(ctx, config.ProjectRoot, config.ExtraArgs)
	if err != nil {
		return err
	}

	args := append([]string{"build"}, config.ExtraArgs...)
	args = cargo.MessageFormatArgs(args)

	cmd := exec.CommandContext(ctx, "cargo", args...)
	cmd.Dir = config.ProjectRoot

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return phelixerr.Wrapf(phelixerr.CodeBuildTimeout, err,
				"cargo build for %s exceeded the %s build timeout; raise it with --build-timeout",
				config.Name, buildTimeout(config.Timeout))
		}

		return phelixerr.Wrapf(
			phelixerr.CodeBuildFailed,
			&ToolError{Tool: "cargo", Output: tailOutput(stderr.Bytes()), Err: err},
			"%s",
			cargoFailureMessage(err, config),
		)
	}

	observeCache(config, RustCacheStatusFromOutput(stderr.String()), buildreport.CacheSourceCargo)

	artifacts, err := cargo.ParseArtifacts(&stdout)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeBuildFailed, err,
			"could not read cargo build messages for %s", config.Name)
	}
	executable, err := cargo.SelectArtifact(artifacts, selection)
	if err != nil {
		return rb.missingArtifactError(config, selection, err)
	}
	if !filepath.IsAbs(executable) {
		executable = filepath.Join(config.ProjectRoot, executable)
	}
	if config.Observe != nil {
		config.Observe.ArtifactPath = executable
	}

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

// missingArtifactError builds the actionable error for "cargo succeeded but
// the selected package and binary produced no executable".
func (rb *RustBuilder) missingArtifactError(config BuildConfig, selection cargo.Selection, cause error) error {
	cache := "unknown"
	if config.Observe != nil {
		cache = string(config.Observe.CacheStatus.Normalized())
	}
	return phelixerr.Wrapf(phelixerr.CodeNotFound, cause,
		"cargo reported success but no executable was produced for package %q target %q (cache %s); "+
			"check --bin/--package or clear target/ if the cache is stale",
		selection.Package.Name, selection.Target.Name, cache)
}

// GetBinaryPath returns the executable cargo reported for the last build. When
// no build observation is available it resolves the same package and binary
// through cargo metadata and checks the exact expected path; it never scans for
// the newest file.
func (rb *RustBuilder) GetBinaryPath(config BuildConfig) (string, error) {
	if config.Observe != nil && config.Observe.ArtifactPath != "" {
		if _, err := os.Stat(config.Observe.ArtifactPath); err != nil {
			return "", phelixerr.Wrapf(phelixerr.CodeNotFound, err,
				"cargo reported executable %s but it is not present", config.Observe.ArtifactPath)
		}
		return config.Observe.ArtifactPath, nil
	}

	meta, err := cargo.LoadMetadata(context.Background(), config.ProjectRoot)
	if err != nil {
		return "", err
	}
	selection, err := cargo.Select(meta, config.ProjectRoot, config.ExtraArgs)
	if err != nil {
		return "", err
	}
	path := cargo.ExecutablePath(
		cargo.TargetDir(meta, config.ProjectRoot, config.ExtraArgs),
		cargo.TargetTriple(config.ExtraArgs),
		cargo.ProfileDir(config.ExtraArgs),
		selection.Target.Name,
	)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if _, err := os.Stat(path + ".exe"); err == nil {
		return path + ".exe", nil
	}
	return "", rb.missingArtifactError(config, selection,
		phelixerr.Newf(phelixerr.CodeNotFound, "expected executable at %s", path))
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
		"--profile",
		"--target-dir",
	}
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

// BuildAndCopyRust builds and copies the selected cargo executable.
func BuildAndCopyRust(config BuildConfig) error {
	if config.Observe == nil {
		config.Observe = &BuildObservation{CacheStatus: buildreport.CacheUnknown}
	}

	rb := NewRustBuilder()

	if err := rb.Build(config); err != nil {
		return err
	}

	binaryPath, err := rb.GetBinaryPath(config)
	if err != nil {
		return err
	}

	return CopyBuiltBinary(binaryPath, config.OutputPath)
}
