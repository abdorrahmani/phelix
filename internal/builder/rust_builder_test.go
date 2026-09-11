package builder

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// fakeCargoDir creates a temp dir with a `cargo` shim script that simulates
// the real compiler (exit code + output) without requiring a Rust toolchain.
func fakeCargoDir(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cargo")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write cargo shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func rustBuildConfig(t *testing.T, root string) BuildConfig {
	return BuildConfig{
		ID:          "test-id",
		Name:        "concurrency-lab-api",
		ProjectRoot: root,
		OutputPath:  filepath.Join(t.TempDir(), "out", "binary"),
		Language:    Rust,
		Observe:     &BuildObservation{CacheStatus: buildreport.CacheUnknown},
	}
}

func writeCargoProject(t *testing.T, cargoToml string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte(cargoToml), 0o644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}
	return root
}

// TestRustBuild_CompilerFailurePreservesRootCause (regression): a failing
// cargo build must surface the exit status and the useful stderr tail, and
// the *exec.ExitError must stay reachable via errors.As. The compiler tail is
// carried on the ToolError (rendered only after redaction), never embedded in
// the error message.
func TestRustBuild_CompilerFailurePreservesRootCause(t *testing.T) {
	fakeCargoDir(t, "echo 'error[E0425]: cannot find value x in this scope' >&2; exit 101")
	root := writeCargoProject(t, "[package]\nname = \"lab\"\nversion = \"0.1.0\"\n")

	cfg := rustBuildConfig(t, root)
	err := NewRustBuilder().Build(cfg)
	if err == nil {
		t.Fatalf("cargo failure must fail the build")
	}

	msg := err.Error()
	if !strings.Contains(msg, "cargo build failed for concurrency-lab-api") {
		t.Fatalf("error message must name the failing build, got: %q", msg)
	}
	if strings.Contains(msg, "error[E0425]") {
		t.Fatalf("compiler output must not enter the error message: %q", msg)
	}

	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("*ToolError must stay reachable via errors.As")
	}
	if toolErr.Tool != "cargo" {
		t.Fatalf("ToolError.Tool = %q, want cargo", toolErr.Tool)
	}
	if !strings.Contains(toolErr.Output, "error[E0425]") {
		t.Fatalf("ToolError output must carry the compiler diagnostic tail: %q", toolErr.Output)
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("*exec.ExitError must stay reachable via errors.As")
	}
	if exitErr.ExitCode() != 101 {
		t.Fatalf("exit code = %d, want 101", exitErr.ExitCode())
	}
}

// TestRustBuild_ToolchainMissingIsActionable: a missing cargo binary must
// produce a clear error, not an empty artifact step.
func TestRustBuild_ToolchainMissingIsActionable(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no cargo anywhere
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")

	err := NewRustBuilder().Build(rustBuildConfig(t, root))
	if err == nil {
		t.Fatalf("missing cargo must fail the build")
	}
	if !strings.Contains(err.Error(), "cargo build failed") {
		t.Fatalf("unexpected message: %q", err.Error())
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("exec.ErrNotFound must stay reachable via errors.Is")
	}
}

// TestRustBuild_MissingBinaryAfterSuccessIsActionable (regression): cargo
// succeeding with no artifact must NOT become a generic BUILD_FAILED; the
// error names the expected path, package, and cache status.
func TestRustBuild_MissingBinaryAfterSuccessIsActionable(t *testing.T) {
	fakeCargoDir(t, `exit 0`)
	root := writeCargoProject(t, "[package]\nname = \"lab\"\nversion = \"0.1.0\"\n")

	cfg := rustBuildConfig(t, root)
	cfg.Observe.CacheStatus = buildreport.CacheHit

	err := NewRustBuilder().Build(cfg)
	if err != nil {
		t.Fatalf("cargo itself succeeded: %v", err)
	}
	_, err = NewRustBuilder().GetBinaryPath(cfg)
	if err == nil {
		t.Fatalf("missing artifact after successful cargo must error")
	}
	msg := err.Error()
	for _, want := range []string{"expected an executable in", "lab", "cache", "[[bin]]"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("artifact error missing %q: %q", want, msg)
		}
	}
}

// TestRustBuild_FallbackPrefersExecutableOverNewerArtifacts (regression):
// target/debug holds .d/.rlib files newer than the binary; the fallback must
// pick an executable, never a dependency artifact.
func TestRustBuild_FallbackPrefersExecutableOverNewerArtifacts(t *testing.T) {
	fakeCargoDir(t, `exit 0`)
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	target := filepath.Join(root, "target", "debug")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	bin := filepath.Join(target, "some-binary")
	if err := os.WriteFile(bin, []byte("elf"), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(bin, stale, stale); err != nil {
		t.Fatalf("chtimes bin: %v", err)
	}
	// Newer files that the old "newest file wins" fallback mistook for binaries.
	for _, name := range []string{"lab.d", "liblab.rlib", "liblab.a"} {
		if err := os.WriteFile(filepath.Join(target, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	cfg := rustBuildConfig(t, root)
	got, err := NewRustBuilder().GetBinaryPath(cfg)
	if err != nil {
		t.Fatalf("GetBinaryPath: %v", err)
	}
	if got != bin {
		t.Fatalf("fallback picked %q, want the executable %q", got, bin)
	}
}

// TestRustBuild_BinNameResolution: a crate whose [[bin]] name differs from
// the package name still resolves via the [[bin]] section.
func TestRustBuild_BinNameResolution(t *testing.T) {
	fakeCargoDir(t, `exit 0`)
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n\n[[bin]]\nname = \"server-bin\"\npath = \"src/main.rs\"\n")
	target := filepath.Join(root, "target", "debug")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := filepath.Join(target, "server-bin")
	if err := os.WriteFile(want, []byte("elf"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := NewRustBuilder().GetBinaryPath(rustBuildConfig(t, root))
	if err != nil {
		t.Fatalf("GetBinaryPath: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want [[bin]]-resolved %q", got, want)
	}
}

// TestRustBuildAndCopy_SuccessCopiesArtifact: the happy path — artifact found
// and copied to the versioned output path.
func TestRustBuildAndCopy_SuccessCopiesArtifact(t *testing.T) {
	fakeCargoDir(t, `exit 0`)
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	target := filepath.Join(root, "target", "debug")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "lab"), []byte("elf-bytes"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := filepath.Join(t.TempDir(), "builds", "v1", "binary")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatalf("mkdir output dir: %v", err)
	}
	cfg := rustBuildConfig(t, root)
	cfg.OutputPath = out
	if err := BuildAndCopyRust(cfg); err != nil {
		t.Fatalf("BuildAndCopyRust: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "elf-bytes" {
		t.Fatalf("copied artifact wrong: %q err=%v", data, err)
	}
}

// TestRustBuildAndCopy_CopyFailureWrapped: an unwritable output location
// fails with a wrapped, cause-preserving error instead of vanishing.
func TestRustBuildAndCopy_CopyFailureWrapped(t *testing.T) {
	fakeCargoDir(t, `exit 0`)
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	target := filepath.Join(root, "target", "debug")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "lab"), []byte("elf"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := rustBuildConfig(t, root)
	cfg.OutputPath = filepath.Join(t.TempDir(), "no-such-dir", "binary")
	err := BuildAndCopyRust(cfg)
	if err == nil {
		t.Fatalf("copy into missing dir must fail")
	}
	if !strings.Contains(err.Error(), "failed to write output binary") {
		t.Fatalf("unexpected copy error: %q", err.Error())
	}
	if !phelixerr.IsCode(err, phelixerr.CodeFilesystem) {
		t.Fatalf("copy error must carry the filesystem code")
	}
}

// TestCopyBuiltBinary_ReplacesRunningExecutable (ETXTBSY regression): the
// output path <source-dir>/app_<id> may be the executable a running instance
// was started from. The copy must replace it via temp+rename (no "text file
// busy"), keep the old process alive on its inode, and leave no temp files.
func TestCopyBuiltBinary_ReplacesRunningExecutable(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "app_running-id")

	// A tiny executable stands in for the running binary.
	if err := os.WriteFile(output, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write running binary: %v", err)
	}
	running, err := os.StartProcess(output, []string{output}, &os.ProcAttr{
		Files: []*os.File{devNullFile(), devNullFile(), devNullFile()},
	})
	if err != nil {
		t.Fatalf("start running binary: %v", err)
	}
	defer func() { _ = running.Kill() }()
	time.Sleep(150 * time.Millisecond)

	source := filepath.Join(dir, "fresh-build")
	if err := os.WriteFile(source, []byte("#!/bin/sh\necho v2\n"), 0o755); err != nil {
		t.Fatalf("write fresh build: %v", err)
	}

	if err := CopyBuiltBinary(source, output); err != nil {
		t.Fatalf("copy over running executable must not fail with ETXTBSY: %v", err)
	}

	if err := running.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("old process must stay alive through the copy: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "#!/bin/sh\necho v2\n" {
		t.Fatalf("output path must now hold the new build: %q err=%v", data, err)
	}
	matches, _ := filepath.Glob(output + ".phelix-tmp-*")
	if len(matches) != 0 {
		t.Fatalf("temp files must be renamed away, found %v", matches)
	}
}

func devNullFile() *os.File {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		panic(err)
	}
	return f
}
