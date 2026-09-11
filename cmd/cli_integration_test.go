package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// --- End-to-end CLI integration tests ---------------------------------------
//
// These tests build the real `phelix` binary once and execute it as a
// subprocess against an isolated $HOME. They pin the CLI boundary contract
// that unit tests cannot: the exit code a script actually sees, and that the
// error is rendered once to stderr (never stdout), with no state pollution
// from the in-process package globals.
//
// Because they exercise a real binary and real HOME, they are also the
// app-not-found and usage-error integration scenarios from the error-handling
// spec: the wire reaches the error boundary exactly as a user's terminal
// would.

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// phelixBin builds the CLI binary once per test run and returns its path.
func phelixBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		repoRoot, err := filepath.Abs("..")
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "phelix-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "phelix")
		build := exec.Command("go", "build", "-o", binPath, ".")
		build.Dir = repoRoot
		out, err := build.CombinedOutput()
		if err != nil {
			buildErr = err
			t.Logf("go build output: %s", out)
		}
	})
	if buildErr != nil {
		t.Fatalf("failed to build phelix binary: %v", buildErr)
	}
	return binPath
}

// runPhelixInHome runs `phelix <args...>` in an isolated HOME and returns its
// exit code, stdout, and stderr.
func runPhelixInHome(t *testing.T, home string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(phelixBin(t), args...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("failed to run phelix %v: %v", args, err)
		}
	}
	return code, stdout.String(), stderr.String()
}

// TestCLIStatus_AppNotFound_Exit12 is the integration pair of the status.go
// over-wrap fix: a missing app must surface as NOT_FOUND (exit 12), not a
// generic PROCESS_FAILED (exit 1).
func TestCLIStatus_AppNotFound_Exit12(t *testing.T) {
	home := t.TempDir()
	// Pre-create ~/.phelix so package init does not print a warning to stdout.
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatalf("mkdir .phelix: %v", err)
	}

	code, stdout, stderr := runPhelixInHome(t, home, "status", "missing-app")
	if code != ExitNotFound {
		t.Fatalf("phelix status missing-app exit = %d, want %d (NOT_FOUND); stdout=%q stderr=%q",
			code, ExitNotFound, stdout, stderr)
	}
	if !strings.Contains(stderr, "NOT_FOUND") {
		t.Fatalf("NOT_FOUND code missing from stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "phelix list") {
		t.Fatalf("actionable hint missing from stderr: %q", stderr)
	}
	// The error must never be rendered to stdout (CLI UX: errors on stderr).
	if stdout != "" {
		t.Fatalf("stdout polluted with error output: %q", stdout)
	}
	// Exactly one rendered error.
	if n := strings.Count(stderr, "Error:"); n != 1 {
		t.Fatalf("expected exactly one 'Error:' on stderr, got %d: %q", n, stderr)
	}
}

// TestCLIUsageError_Exit2 pins that a bad invocation (a missing required
// argument) renders INVALID_ARGUMENT to stderr and exits 2, so scripts can
// distinguish "bad invocation" from "operation failed".
func TestCLIUsageError_Exit2(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatalf("mkdir .phelix: %v", err)
	}

	code, stdout, stderr := runPhelixInHome(t, home, "status")
	if code != ExitUsage {
		t.Fatalf("phelix status (no args) exit = %d, want %d (USAGE); stdout=%q stderr=%q",
			code, ExitUsage, stdout, stderr)
	}
	if !strings.Contains(stderr, "INVALID_ARGUMENT") {
		t.Fatalf("expected INVALID_ARGUMENT on stderr: %q", stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout polluted with error output: %q", stdout)
	}
}

// TestCLIStatus_OutputShape sanity-checks the plain non-error status shape:
// a real error line must not appear, and the exit code must be 0 on success.
// To produce a success we seed a minimal apps.json whose status is stopped (no
// process verification needed), then `phelix status <id>` should render.
func TestCLIStatus_StoppedApp_Succeeds(t *testing.T) {
	home := t.TempDir()
	phelixDir := filepath.Join(home, ".phelix")
	if err := os.MkdirAll(filepath.Join(phelixDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// apps.json with one stopped app; StopApplication clears the process, so
	// status verification is skipped for non-running apps.
	appsJSON := `{
  "203": {
    "id": "203",
    "name": "demo",
    "pid": 0,
    "status": "stopped",
    "built_status": "built"
  }
}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps.json"), []byte(appsJSON), 0o644); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}

	code, stdout, stderr := runPhelixInHome(t, home, "status", "203")
	if code != 0 {
		t.Fatalf("phelix status <stopped id> exit = %d, want 0; stderr=%q", code, stderr)
	}
	// The table renders to stdout; no error text anywhere.
	if strings.Contains(stdout, "Error:") || strings.Contains(stderr, "Error:") {
		t.Fatalf("unexpected error rendering on success: stdout=%q stderr=%q", stdout, stderr)
	}
	if !regexp.MustCompile(`(?m)demo`).MatchString(stdout) {
		t.Fatalf("expected app name in status output: %q", stdout)
	}
}
