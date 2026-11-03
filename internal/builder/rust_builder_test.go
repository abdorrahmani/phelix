package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/cargo"
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

// fakeCargoWith installs a cargo shim that answers metadata and build calls
// from fixed JSON fixtures.
func fakeCargoWith(t *testing.T, metadata string, artifacts []string, exitCode int, stderr string) {
	t.Helper()
	dir := t.TempDir()
	metaPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(metaPath, []byte(metadata), 0o644); err != nil {
		t.Fatalf("write metadata fixture: %v", err)
	}
	var stdout strings.Builder
	for _, artifact := range artifacts {
		stdout.WriteString(artifact)
		stdout.WriteString("\n")
	}
	stdoutPath := filepath.Join(dir, "stdout.json")
	if err := os.WriteFile(stdoutPath, []byte(stdout.String()), 0o644); err != nil {
		t.Fatalf("write artifact fixture: %v", err)
	}
	stderrPath := filepath.Join(dir, "stderr.txt")
	if err := os.WriteFile(stderrPath, []byte(stderr), 0o644); err != nil {
		t.Fatalf("write stderr fixture: %v", err)
	}

	script := fmt.Sprintf(`if [ "$1" = "metadata" ]; then
  cat %q
  exit 0
fi
cat %q >&2
cat %q
exit %d`, metaPath, stderrPath, stdoutPath, exitCode)
	fakeCargoDir(t, script)
}

func testMetadata(t *testing.T, workspaceRoot, targetDir string, packages ...cargo.Package) string {
	t.Helper()
	members := make([]string, 0, len(packages))
	for _, pkg := range packages {
		members = append(members, pkg.ID)
	}
	meta := cargo.Metadata{
		Packages:                packages,
		WorkspaceMembers:        members,
		WorkspaceDefaultMembers: members,
		TargetDirectory:         targetDir,
		WorkspaceRoot:           workspaceRoot,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	return string(data)
}

func testPackage(id, name, manifest, defaultRun string, bins ...string) cargo.Package {
	targets := make([]cargo.Target, 0, len(bins))
	for _, bin := range bins {
		targets = append(targets, cargo.Target{Kind: []string{"bin"}, Name: bin})
	}
	return cargo.Package{
		ID:           id,
		Name:         name,
		ManifestPath: manifest,
		DefaultRun:   defaultRun,
		Targets:      targets,
	}
}

func testArtifact(t *testing.T, packageID, bin, executable string, fresh bool) string {
	t.Helper()
	artifact := cargo.Artifact{
		Reason:     "compiler-artifact",
		PackageID:  packageID,
		Target:     cargo.Target{Kind: []string{"bin"}, Name: bin},
		Executable: &executable,
		Fresh:      fresh,
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	return string(data)
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

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRustBuild_CompilerFailurePreservesRootCause (regression): a failing
// cargo build must surface the exit status and the useful stderr tail, and
// the *exec.ExitError must stay reachable via errors.As.
func TestRustBuild_CompilerFailurePreservesRootCause(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\nversion = \"0.1.0\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg), nil, 101,
		"error[E0425]: cannot find value x in this scope")

	err := NewRustBuilder().Build(rustBuildConfig(t, root))
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

// TestRustBuild_StreamsStderrToOutput: cargo's human-readable status lines
// reach the configured Output while the build still parses the JSON artifact
// stream from stdout and keeps the stderr tail for failure reporting.
func TestRustBuild_StreamsStderrToOutput(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\nversion = \"0.1.0\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	want := filepath.Join(root, "target", "debug", "lab")
	writeExecutable(t, want, "elf")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "lab", want, false)}, 0,
		"   Compiling lab v0.1.0\n    Finished dev profile\n")

	cfg := rustBuildConfig(t, root)
	var out bytes.Buffer
	cfg.Output = &out

	if err := NewRustBuilder().Build(cfg); err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, line := range []string{"Compiling lab v0.1.0", "Finished dev profile"} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("live output missing %q: %q", line, out.String())
		}
	}
	if cfg.Observe.ArtifactPath != want {
		t.Fatalf("artifact path = %q, want %q", cfg.Observe.ArtifactPath, want)
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
	if !strings.Contains(err.Error(), "cargo is required") {
		t.Fatalf("unexpected message: %q", err.Error())
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("exec.ErrNotFound must stay reachable via errors.Is")
	}
}

// TestRustBuild_MissingBinaryAfterSuccessIsActionable (regression): cargo
// succeeding without the selected artifact must not become a generic
// BUILD_FAILED; the error names the package and target.
func TestRustBuild_MissingBinaryAfterSuccessIsActionable(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\nversion = \"0.1.0\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg), nil, 0, "")

	cfg := rustBuildConfig(t, root)
	cfg.Observe.CacheStatus = buildreport.CacheHit

	err := NewRustBuilder().Build(cfg)
	if err == nil {
		t.Fatalf("missing artifact after successful cargo must error")
	}
	msg := err.Error()
	for _, want := range []string{"no executable", "lab", "target", "cache"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("artifact error missing %q: %q", want, msg)
		}
	}
}

// TestRustBuild_UsesSelectedArtifactBesideStaleExecutable (regression): only
// the compiler-artifact executable for the selected package and bin is copied;
// a newer stale executable in the same directory is ignored.
func TestRustBuild_UsesSelectedArtifactBesideStaleExecutable(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	selected := filepath.Join(root, "target", "debug", "lab")
	stale := filepath.Join(root, "target", "debug", "old-lab")
	writeExecutable(t, selected, "selected-bytes")
	writeExecutable(t, stale, "stale-bytes")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(stale, future, future); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}

	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "lab", selected, false)}, 0, "")

	cfg := rustBuildConfig(t, root)
	cfg.OutputPath = filepath.Join(t.TempDir(), "builds", "v1", "binary")
	if err := os.MkdirAll(filepath.Dir(cfg.OutputPath), 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	if err := BuildAndCopyRust(cfg); err != nil {
		t.Fatalf("BuildAndCopyRust: %v", err)
	}
	data, err := os.ReadFile(cfg.OutputPath)
	if err != nil || string(data) != "selected-bytes" {
		t.Fatalf("copied artifact = %q, err=%v; want the selected executable", data, err)
	}
}

// TestRustBuild_WorkspaceMemberUsesWorkspaceTarget: a crate inside a Cargo
// workspace builds into the workspace root's target dir, and the artifact
// message is authoritative regardless of the crate directory.
func TestRustBuild_WorkspaceMemberUsesWorkspaceTarget(t *testing.T) {
	root := writeCargoProject(t, "[workspace]\nmembers = [\"member\"]\n")
	member := filepath.Join(root, "member")
	if err := os.MkdirAll(member, 0o755); err != nil {
		t.Fatalf("mkdir member: %v", err)
	}
	manifest := filepath.Join(member, "Cargo.toml")
	if err := os.WriteFile(manifest, []byte("[package]\nname = \"lab\"\nversion = \"0.1.0\"\n"), 0o644); err != nil {
		t.Fatalf("write member Cargo.toml: %v", err)
	}
	pkg := testPackage("pkg-lab", "lab", manifest, "", "lab")
	want := filepath.Join(root, "target", "debug", "lab")
	writeExecutable(t, want, "elf")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "lab", want, false)}, 0, "")

	cfg := rustBuildConfig(t, member)
	if err := NewRustBuilder().Build(cfg); err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := NewRustBuilder().GetBinaryPath(cfg)
	if err != nil {
		t.Fatalf("GetBinaryPath: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want the workspace target binary %q", got, want)
	}
}

// TestRustBuild_RenamedBinUsesArtifactTarget: a crate whose [[bin]] name
// differs from the package name resolves the exact target cargo built.
func TestRustBuild_RenamedBinUsesArtifactTarget(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n\n[[bin]]\nname = \"server-bin\"\npath = \"src/main.rs\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "server-bin")
	want := filepath.Join(root, "target", "debug", "server-bin")
	writeExecutable(t, want, "elf")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "server-bin", want, false)}, 0, "")

	cfg := rustBuildConfig(t, root)
	if err := NewRustBuilder().Build(cfg); err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := NewRustBuilder().GetBinaryPath(cfg)
	if err != nil {
		t.Fatalf("GetBinaryPath: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want [[bin]]-resolved %q", got, want)
	}
}

// TestRustBuild_CustomProfileUsesArtifact: --profile production selects the
// profile directory cargo reports, not a hard-coded debug/release path.
func TestRustBuild_CustomProfileUsesArtifact(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	want := filepath.Join(root, "target", "production", "lab")
	writeExecutable(t, want, "elf")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "lab", want, false)}, 0, "")

	cfg := rustBuildConfig(t, root)
	cfg.ExtraArgs = []string{"--profile", "production"}
	if err := NewRustBuilder().Build(cfg); err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := NewRustBuilder().GetBinaryPath(cfg)
	if err != nil {
		t.Fatalf("GetBinaryPath: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want profile artifact %q", got, want)
	}
}

// TestRustBuild_CustomTargetDirUsesArtifact: --target-dir moves the output;
// the artifact path wins over the metadata target directory.
func TestRustBuild_CustomTargetDirUsesArtifact(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	custom := filepath.Join(t.TempDir(), "custom-target")
	want := filepath.Join(custom, "debug", "lab")
	writeExecutable(t, want, "elf")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "lab", want, false)}, 0, "")

	cfg := rustBuildConfig(t, root)
	cfg.ExtraArgs = []string{"--target-dir", custom}
	if err := NewRustBuilder().Build(cfg); err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := NewRustBuilder().GetBinaryPath(cfg)
	if err != nil {
		t.Fatalf("GetBinaryPath: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want custom target-dir artifact %q", got, want)
	}
}

// TestRustBuild_TargetTripleUsesArtifact: --target inserts the triple
// directory; the artifact path is used verbatim.
func TestRustBuild_TargetTripleUsesArtifact(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	triple := "aarch64-unknown-linux-gnu"
	want := filepath.Join(root, "target", triple, "debug", "lab")
	writeExecutable(t, want, "elf")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "lab", want, false)}, 0, "")

	cfg := rustBuildConfig(t, root)
	cfg.ExtraArgs = []string{"--target", triple}
	if err := NewRustBuilder().Build(cfg); err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := NewRustBuilder().GetBinaryPath(cfg)
	if err != nil {
		t.Fatalf("GetBinaryPath: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want triple artifact %q", got, want)
	}
}

// TestRustBuild_CacheFreshArtifact: cargo emits a compiler-artifact with
// fresh=true when nothing was recompiled; the executable is still resolved.
func TestRustBuild_CacheFreshArtifact(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	want := filepath.Join(root, "target", "debug", "lab")
	writeExecutable(t, want, "cached-elf")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "lab", want, true)}, 0, "")

	cfg := rustBuildConfig(t, root)
	cfg.OutputPath = filepath.Join(t.TempDir(), "builds", "v1", "binary")
	if err := os.MkdirAll(filepath.Dir(cfg.OutputPath), 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	if err := BuildAndCopyRust(cfg); err != nil {
		t.Fatalf("BuildAndCopyRust: %v", err)
	}
	data, err := os.ReadFile(cfg.OutputPath)
	if err != nil || string(data) != "cached-elf" {
		t.Fatalf("copied cache-fresh artifact wrong: %q err=%v", data, err)
	}
}

// TestRustBuildAndCopy_CopyFailureWrapped: an unwritable output location
// fails with a wrapped, cause-preserving error instead of vanishing.
func TestRustBuildAndCopy_CopyFailureWrapped(t *testing.T) {
	root := writeCargoProject(t, "[package]\nname = \"lab\"\n")
	pkg := testPackage("pkg-lab", "lab", filepath.Join(root, "Cargo.toml"), "", "lab")
	want := filepath.Join(root, "target", "debug", "lab")
	writeExecutable(t, want, "elf")
	fakeCargoWith(t, testMetadata(t, root, filepath.Join(root, "target"), pkg),
		[]string{testArtifact(t, pkg.ID, "lab", want, false)}, 0, "")

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
// was started from. The copy must replace it via temp+rename.
func TestCopyBuiltBinary_ReplacesRunningExecutable(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "app_running-id")

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
