package matrix

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// RustMatrixBuilder handles Rust cross-compilation via the `cross` tool.
//
// Why `cross` instead of raw `rustup target add` + `cargo build --target`:
//
// Cargo's built-in cross-compilation (`cargo build --target <triple>`) only
// compiles Rust code. It does NOT handle C/C++ dependencies, linkers, or
// system libraries. For projects that depend on crates using build scripts
// (cc, cmake, pkg-config, openssl-sys, etc.), `cargo build --target` fails
// with cryptic linker errors because:
//
//  1. Each target triple needs a matching linker (e.g. aarch64-linux-gnu-gcc
//     for arm64 Linux) installed and configured in .cargo/config.toml.
//  2. C/C++ cross-compilation libraries (libssl-dev:arm64, etc.) must be
//     installed for the target — these don't exist on most build hosts.
//  3. Even pure-Rust crates sometimes depend on system libraries via build
//     scripts (e.g. linking to libc, zlib, or other shared libs).
//
// The `cross` tool (https://github.com/cross-rs/cross) solves this by
// running each build inside a Docker container that has:
//   - The correct target triple's linker pre-configured
//   - C/C++ cross-compilation toolchains pre-installed
//   - Common system libraries available for the target
//   - The exact Rust toolchain version specified
//
// This means `cross build --target aarch64-unknown-linux-gnu` "just works"
// for any crate, including those with C dependencies, without the user
// having to manually configure cross-compilers.
//
// Tradeoff: `cross` requires Docker to be running. If Docker is unavailable,
// we fail with a clear error rather than falling back to `cargo build --target`
// which would silently fail for any project with C dependencies.
type RustMatrixBuilder struct {
	ProjectRoot string
	AppName     string
	Debug       bool
}

// CheckCrossInstalled verifies that the `cross` tool is available.
func CheckCrossInstalled() error {
	_, err := exec.LookPath("cross")
	if err != nil {
		// Preserve the exec.LookPath cause (os.IsNotExist) for root-cause inspection.
		return phelixerr.Wrapf(
			phelixerr.CodeToolchainNotFound,
			err,
			"the `cross` tool is required for Rust cross-compilation but was not found. "+
				"Install it with: cargo install cross --git https://github.com/cross-rs/cross — "+
				"then ensure Docker is running (cross builds inside Docker containers).")
	}
	return nil
}

// BuildRust cross-compiles a Rust project for the given combination using `cross`.
//
// The build command:
//
//	cross build --target <triple> --release --manifest-path Cargo.toml
//
// Output is extracted from target/<triple>/release/<binary-name>.
// Each combination gets its own cache directory so different targets don't
// invalidate each other's incremental compilation cache.
func (r *RustMatrixBuilder) Build(ctx context.Context, c Combination) *Result {
	result := &Result{Combination: c}

	logLine := func(format string, args ...any) {
		if r.Debug {
			result.Log += fmt.Sprintf(format, args...) + "\n"
		}
	}

	start := time.Now()

	// Verify `cross` is installed.
	if err := CheckCrossInstalled(); err != nil {
		result.Status = "failed"
		result.Error = err
		logLine("error: cross not installed")
		return result
	}

	// Map our platform string to a Rust target triple.
	triple, err := platformToRustTriple(c.OS, c.Arch)
	if err != nil {
		result.Status = "failed"
		result.Error = err
		return result
	}

	// Determine the binary name from Cargo.toml.
	binName, err := r.getPkgName()
	if err != nil {
		result.Status = "failed"
		result.Error = err
		return result
	}

	// Per-combination target directory to avoid cache collisions.
	targetDir := filepath.Join(r.ProjectRoot, "target", c.ID())
	outDir := filepath.Join(r.ProjectRoot, "builds", "matrix", c.ID())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		result.Status = "failed"
		result.Error = phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create output dir")
		return result
	}

	logLine("triple:   %s", triple)
	logLine("binary:   %s", binName)
	logLine("target:   %s", targetDir)
	logLine("output:   %s", outDir)
	logLine("command:  cross build --target %s --release --target-dir %s", triple, targetDir)

	cmd := exec.CommandContext(ctx,
		"cross", "build",
		"--target", triple,
		"--release",
		"--target-dir", targetDir,
	)
	cmd.Dir = r.ProjectRoot

	output := &bytes.Buffer{}
	commandOutput := &progressOutputWriter{ctx: ctx, key: c.ID(), debug: r.Debug, log: output}
	ReportBuildProgress(ctx, c.ID(), "preparing", 0, 0)
	cmd.Stdout = commandOutput
	cmd.Stderr = commandOutput
	if err := cmd.Run(); err != nil {
		result.Status = "failed"
		// Keep the *exec.ExitError reachable; compiler output goes to the debug log.
		result.Error = phelixerr.Wrapf(phelixerr.CodeBuildFailed, err, "cross build failed for %s (target %s)", r.AppName, triple)
		logLine("error: %v", err)
		if s := output.String(); s != "" {
			logLine("stderr: %s", s)
		}
		return result
	}

	// Locate the built binary.
	binPath := filepath.Join(targetDir, triple, "release", binName)
	if _, err := os.Stat(binPath); err != nil {
		// Try with .exe suffix (Windows target).
		binPath += ".exe"
		if _, err := os.Stat(binPath); err != nil {
			result.Status = "failed"
			result.Error = phelixerr.Newf(phelixerr.CodeNotFound, "built binary not found at expected path for %s (target %s)", r.AppName, triple)
			return result
		}
	}

	// Copy to the output directory.
	outBin := filepath.Join(outDir, c.BinaryName(r.AppName))
	if err := copyBinary(binPath, outBin); err != nil {
		result.Status = "failed"
		result.Error = phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "copy binary")
		return result
	}

	result.Duration = time.Since(start)
	result.Status = "success"
	result.Artifact = outBin
	return result
}

// getPkgName reads the binary name from Cargo.toml.
func (r *RustMatrixBuilder) getPkgName() (string, error) {
	cargoPath := filepath.Join(r.ProjectRoot, "Cargo.toml")
	data, err := os.ReadFile(cargoPath)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "read Cargo.toml")
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name") && strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return strings.Trim(strings.TrimSpace(parts[1]), "\"'"), nil
			}
		}
	}
	return "", phelixerr.New(phelixerr.CodeUnsupportedProject, "could not find package name in Cargo.toml")
}

// platformToRustTriple converts an os/arch pair to a Rust target triple.
func platformToRustTriple(goOS, goArch string) (string, error) {
	// Mapping from GOOS/GOARCH to Rust target triples.
	// These are the most common targets. For exotic combinations,
	// users should use `rustup target list` to find the triple.
	triples := map[string]string{
		"linux/amd64":   "x86_64-unknown-linux-gnu",
		"linux/arm64":   "aarch64-unknown-linux-gnu",
		"linux/arm/v7":  "armv7-unknown-linux-gnueabihf",
		"linux/arm/v6":  "arm-unknown-linux-gnueabihf",
		"darwin/amd64":  "x86_64-apple-darwin",
		"darwin/arm64":  "aarch64-apple-darwin",
		"windows/amd64": "x86_64-pc-windows-msvc",
	}

	key := goOS + "/" + goArch
	if triple, ok := triples[key]; ok {
		return triple, nil
	}
	return "", phelixerr.Newf(
		phelixerr.CodeInvalidArgument,
		"no Rust target triple mapping for platform %s/%s — "+
			"check `rustup target list` for available targets", goOS, goArch)
}

func copyBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
