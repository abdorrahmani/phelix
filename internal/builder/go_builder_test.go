package builder

import (
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

	// Start the built binary — this is the "previous version still running".
	running, err := os.StartProcess(output, []string{output}, &os.ProcAttr{
		Files: []*os.File{devNull(), devNull(), devNull()},
	})
	if err != nil {
		t.Fatalf("start built binary: %v", err)
	}
	defer func() { _ = running.Kill() }()
	time.Sleep(150 * time.Millisecond)

	// Rebuild over the running executable: with a direct `go build -o` this
	// fails with "text file busy" (ETXTBSY).
	if err := gb.Build(cfg); err != nil {
		t.Fatalf("rebuild over running executable must not fail: %v", err)
	}

	// The running instance must have survived the rebuild untouched.
	if err := running.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("old process must stay alive through the rebuild: %v", err)
	}
	// And no temp file may be left behind.
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
