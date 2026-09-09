package matrix

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
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
// with cryptic linker errors.
//
// The `cross` tool (https://github.com/cross-rs/cross) solves this by
// running each build inside a Docker container that has the target triple's
// linker pre-configured, C/C++ cross-compilation toolchains pre-installed,
// and common system libraries available for the target.
//
// Toolchain pinning: the requested Rust version is passed as
// `cross +<version> build ...`. `cross` (like cargo) is a rustup proxy, so
// `+<version>` selects — and auto-installs when missing — the exact
// toolchain. Without this, every version in --rust-versions would silently
// build with the host's default toolchain and the report would misattribute
// the artifacts.
type RustMatrixBuilder struct {
	ProjectRoot string
	AppName     string
	Debug       bool
	// ExtraArgs are appended verbatim to `cross build` (e.g. --locked).
	ExtraArgs []string
	// LookPath resolves executables on the host. Overridable for tests.
	LookPath func(string) (string, error)
	// CommandContext creates executed commands. Overridable for tests.
	CommandContext func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func (r *RustMatrixBuilder) lookPath(name string) (string, error) {
	if r.LookPath != nil {
		return r.LookPath(name)
	}
	return exec.LookPath(name)
}

func (r *RustMatrixBuilder) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if r.CommandContext != nil {
		return r.CommandContext(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
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

func (r *RustMatrixBuilder) checkCrossInstalled() error {
	if _, err := r.lookPath("cross"); err != nil {
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
//	cross +<version> build --target <triple> --release --target-dir <dir> [extra...]
//
// Output is extracted from target/<id>/<triple>/release/<binary-name>.
// Each combination gets its own target directory so different toolchains and
// targets don't invalidate each other's incremental compilation cache.
func (r *RustMatrixBuilder) Build(ctx context.Context, c Combination) *Result {
	result := &Result{Combination: c}

	logLine := func(format string, args ...any) {
		if r.Debug {
			result.Log += fmt.Sprintf(format, args...) + "\n"
		}
	}

	start := time.Now()

	// Verify `cross` is installed.
	if err := r.checkCrossInstalled(); err != nil {
		result.Status = "failed"
		result.Error = err
		logLine("error: cross not installed")
		return result
	}

	// Map our platform string to a Rust target triple (ARM variants included).
	triple, err := platformToRustTriple(c.OS, archWithVariant(c))
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
	logLine("command:  cross +%s build --target %s --release --target-dir %s", c.Version, triple, targetDir)

	args := []string{"+" + c.Version, "build", "--target", triple, "--release", "--target-dir", targetDir}
	args = append(args, r.ExtraArgs...)
	cmd := r.command(ctx, "cross", args...)
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
	result.CacheStatus = string(builder.RustCacheStatusFromOutput(output.String()))
	return result
}

// archWithVariant renders the arch including any ARM variant so platform
// lookups see "arm/v7" rather than bare "arm".
func archWithVariant(c Combination) string {
	if c.Variant == "" {
		return c.Arch
	}
	return c.Arch + "/" + c.Variant
}

// getPkgName determines the binary `cargo build` produces, from Cargo.toml.
// Precedence: default-run (if set), the single [[bin]] target's name, then
// the [package] name. A naive "first name= line" parse would return the
// package name even when the binary is named differently (or pick up an
// unrelated name= key in another section).
func (r *RustMatrixBuilder) getPkgName() (string, error) {
	cargoPath := filepath.Join(r.ProjectRoot, "Cargo.toml")
	data, err := os.ReadFile(cargoPath)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "read Cargo.toml")
	}

	section := ""
	var pkgName, defaultRun string
	var binNames []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.TrimSpace(line)
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if key != "name" && key != "default-run" {
			continue
		}
		switch {
		case section == "[package]" && key == "name":
			pkgName = value
		case section == "[package]" && key == "default-run":
			defaultRun = value
		case section == "[[bin]]" && key == "name":
			binNames = append(binNames, value)
		}
	}

	switch {
	case defaultRun != "":
		return defaultRun, nil
	case len(binNames) == 1:
		return binNames[0], nil
	case pkgName != "":
		return pkgName, nil
	}
	return "", phelixerr.New(phelixerr.CodeUnsupportedProject, "could not find package name in Cargo.toml")
}

// platformToRustTriple converts an os/arch pair (arch optionally carrying an
// ARM variant, e.g. "arm/v7") to a Rust target triple.
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
		"no Rust target triple mapping for platform %s — "+
			"check `rustup target list` for available targets", key)
}

func copyBinary(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}

// sortedKeys returns the map's keys in sorted order so generated command
// lines are deterministic (required for reproducible builds and tests).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
