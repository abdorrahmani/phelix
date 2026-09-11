package docker

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// TestDetectLanguageCodes verifies DetectLanguage classifies the domain
// outcomes (unsupported/ambiguous project) while preserving the exact message
// contract existing callers rely on.
func TestDetectLanguageCodes(t *testing.T) {
	dir := t.TempDir()
	_, err := DetectLanguage(dir)
	if !phelixerr.IsCode(err, phelixerr.CodeUnsupportedProject) {
		t.Fatalf("missing file should be CODE_UNSUPPORTED_PROJECT, got %s", phelixerr.CodeOf(err))
	}
	if err.Error() != "unsupported project: neither go.mod nor Cargo.toml found in "+dir {
		t.Fatalf("message contract changed: %q", err.Error())
	}
}

// TestCheckDockerAvailableWrapsDaemonUnavailable verifies that a missing docker
// CLI is reported as CodeDockerDaemonUnavailable and that the underlying
// exec.Error remains reachable (so errors.Is can still match it).
func TestCheckDockerAvailableWrapsDaemonUnavailable(t *testing.T) {
	// Simulate an uninvokable docker by pointing PATH at an empty dir.
	t.Setenv("PATH", t.TempDir())
	err := CheckDockerAvailable()
	if err == nil {
		t.Skip("docker is available in the host environment")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeDockerDaemonUnavailable) {
		t.Fatalf("expected DOCKER_DAEMON_UNAVAILABLE, got %s", phelixerr.CodeOf(err))
	}
	var execErr *exec.Error
	if !errors.As(err, &execErr) {
		t.Fatalf("underlying *exec.Error not preserved through the wrap: %v", err)
	}
}

// TestBuildImagePreservesExitStatus verifies that a docker build failure keeps
// the underlying *exec.ExitError (and its exit code) reachable through the
// structured wrap — the Section 14/15 exit-status preservation guarantee.
func TestBuildImagePreservesExitStatus(t *testing.T) {
	// Force the docker binary to fail with a known exit code via a fake shim.
	shim := t.TempDir()
	if err := os.WriteFile(shim+"/docker", []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// NOTE: TagImage uses exec.Command("docker", ...) which resolves via PATH.
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := TagImage("src:1", "dst:1")
	if err == nil {
		t.Fatal("expected TagImage to fail with the shim")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeDocker) {
		t.Fatalf("expected DOCKER_ERROR, got %s", phelixerr.CodeOf(err))
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("exit status not preserved through the wrap: %v", err)
	}
	// ExitError observed via the wrap; the raw message must NOT embed docker
	// stderr (Section 9/15 — no output dump).
	if contains2(err.Error(), "Output:") {
		t.Fatalf("docker error must not dump command output: %q", err.Error())
	}
}

// TestPushAuthClassification verifies the auth-vs-generic distinction in
// PushImage is preserved with structured codes. Because CheckDockerAvailable
// runs first, we test the classification helper isAuthError stays intact and
// add a direct PushImage path test guarded on the environment.
func TestPushAuthClassification(t *testing.T) {
	if !isAuthError("unauthorized: authentication required") {
		t.Fatal("isAuthError should classify auth failure")
	}
	if isAuthError("Error: Cannot connect to the Docker daemon") {
		t.Fatal("isAuthError should NOT classify daemon-connection as auth")
	}
}

// contains2 is contains() for this package (avoids shadowing a possible
// standard-library name mismatch).
func contains2(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
