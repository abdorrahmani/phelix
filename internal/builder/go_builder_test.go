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
)

// TestGoBuild_ReplacesRunningExecutable (ETXTBSY regression): rebuilding while
// the previous binary is still executing must succeed via temp+rename, keep
// the running process alive, and leave the new binary at the output path.
func TestGoBuild_ReplacesRunningExecutable(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() { select {} }\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	output := filepath.Join(root, "app_test-id")
	cfg := BuildConfig{
		ID:          "test-id",
		Name:        "goapp",
		ProjectRoot: root,
		OutputPath:  output,
		Language:    Go,
		Observe:     &BuildObservation{},
	}

	gb := NewGoBuilder()
	if err := gb.Build(cfg); err != nil {
		t.Fatalf("first build: %v", err)
	}

	running, err := os.StartProcess(output, []string{output}, &os.ProcAttr{
		Files: []*os.File{devNull(), devNull(), devNull()},
	})
	if err != nil {
		t.Fatalf("start built binary: %v", err)
	}
	defer func() { _ = running.Kill() }()
	time.Sleep(150 * time.Millisecond)

	if err := gb.Build(cfg); err != nil {
		t.Fatalf("rebuild over running executable must not fail: %v", err)
	}

	if err := running.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("old process must stay alive through the rebuild: %v", err)
	}

	matches, _ := filepath.Glob(output + ".phelix-tmp-*")
	if len(matches) != 0 {
		t.Fatalf("temp files must be renamed away, found %v", matches)
	}
}

func devNull() *os.File {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		panic(err)
	}
	return f
}

func TestGetBinaryPath_MissingOutputIsActionable(t *testing.T) {
	root := t.TempDir()
	cfg := BuildConfig{ProjectRoot: root, OutputPath: filepath.Join(root, "missing"), Language: Go}
	_, err := NewGoBuilder().GetBinaryPath(cfg)
	if err == nil || !strings.Contains(err.Error(), "built binary not found") {
		t.Fatalf("expected actionable missing-binary error, got %v", err)
	}
}

// TestGoBuild_MultiFilePackage is the build the old target could not produce:
// two files in package main, the helper in the second one. Handing `go build`
// the path of main.go compiled it as an ad-hoc command-line-arguments package
// containing only that file, so the helper in util.go came back as
// "undefined: helper", a wrong answer about the user's own code.
func TestGoBuild_MultiFilePackage(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/multi\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "main.go"),
		"package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(helper()) }\n")
	writeTestFile(t, filepath.Join(root, "util.go"),
		"package main\n\nfunc helper() string { return \"ok\" }\n")

	cfg := BuildConfig{
		ID:          "multi",
		Name:        "multi",
		ProjectRoot: root,
		OutputPath:  filepath.Join(root, "app_multi"),
		Language:    Go,
		Observe:     &BuildObservation{},
	}

	if err := NewGoBuilder().Build(cfg); err != nil {
		if te, ok := errors.AsType[*ToolError](err); ok {
			t.Fatalf("multi-file package must build: %v\n%s", err, te.Output)
		}
		t.Fatalf("multi-file package must build: %v", err)
	}
	if _, err := os.Stat(cfg.OutputPath); err != nil {
		t.Fatalf("no binary at %s: %v", cfg.OutputPath, err)
	}
}
