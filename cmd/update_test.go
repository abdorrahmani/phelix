package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// --- `phelix update` subprocess tests ----------------------------------------
//
// These run the real binary against a local release server (via
// PHELIX_RELEASE_BASE_URL) so no test depends on phelix.anophel.com being
// reachable. Each test builds its own stamped binaries because `phelix
// update` replaces its own executable — sharing phelixBin would corrupt it
// for other tests.

// testCmd is the single exec point of this test harness. Executables are
// named as literals: "go" builds the CLI, "phelix" runs the test-built CLI,
// which PATH resolution binds to the directory passed to runViaPath.
func testCmd(name string, args ...string) (*exec.Cmd, error) {
	switch name {
	case "go":
		return exec.Command("go", args...), nil
	case "phelix":
		return exec.Command("phelix", args...), nil
	}
	return nil, fmt.Errorf("testCmd: command %q is not allowed", name)
}

var (
	stampedBuildOnce sync.Once
	stampedBinDir    string // holds "phelix" (v1.0.0) and the 1.0.1 release payload
	releasePayload   string // the 1.0.1 binary, served as the release artifact
	stampedBuildErr  error
)

func buildStampedBinaries(t *testing.T) string {
	t.Helper()
	stampedBuildOnce.Do(func() {
		repoRoot, err := filepath.Abs("..")
		if err != nil {
			stampedBuildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "phelix-update-e2e")
		if err != nil {
			stampedBuildErr = err
			return
		}
		stampedBinDir = dir
		releasePayload = filepath.Join(dir, "phelix-release-1.0.1")

		build := func(version, out string) error {
			ldflags := fmt.Sprintf("-X github.com/abdorrahmani/phelix/internal/version.Version=%s", version)
			cmd, err := testCmd("go", "build", "-ldflags", ldflags, "-o", out, ".")
			if err != nil {
				return err
			}
			cmd.Dir = repoRoot
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("go build %s: %v: %s", version, err, output)
			}
			return nil
		}
		if err := build("1.0.0", filepath.Join(dir, "phelix")); err != nil {
			stampedBuildErr = err
			return
		}
		if err := build("1.0.1", releasePayload); err != nil {
			stampedBuildErr = err
			return
		}
	})
	if stampedBuildErr != nil {
		t.Fatalf("failed to build stamped binaries: %v", stampedBuildErr)
	}
	return stampedBinDir
}

// releaseTestServer speaks the installer's release contract for the CLI
// subprocess: latest/version, artifact, and optional .sha256 sidecar.
func releaseTestServer(t *testing.T, latest string, binary []byte, checksum string) *httptest.Server {
	t.Helper()
	artifact := fmt.Sprintf("/releases/%s/phelix-%s-%s", latest, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Phelix-Version", latest)
		fmt.Fprintln(w, latest)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			if checksum == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, checksum)
		case r.URL.Path == artifact:
			w.Write(binary)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runViaPath runs the CLI named "phelix" from binDir (the test-built binary
// that `phelix update` will replace) with an isolated HOME and the given
// release server, returning exit code, stdout, and stderr.
func runViaPath(t *testing.T, binDir, home, baseURL string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd, err := testCmd("phelix", args...)
	if err != nil {
		t.Fatalf("testCmd: %v", err)
	}
	cmd.Env = append(os.Environ(), "HOME="+home, "PHELIX_RELEASE_BASE_URL="+baseURL)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return ee.ExitCode(), stdout.String(), stderr.String()
		}
		t.Fatalf("failed to run phelix %v: %v", args, runErr)
	}
	return 0, stdout.String(), stderr.String()
}

// freshInstalledBinary stages a pristine copy of the stamped 1.0.0 binary
// into a per-test directory, so tests that replace it never corrupt the
// shared build output for each other.
func freshInstalledBinary(t *testing.T) string {
	t.Helper()
	buildStampedBinaries(t)
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join(stampedBinDir, "phelix"))
	if err != nil {
		t.Fatalf("read stamped binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "phelix"), data, 0o755); err != nil {
		t.Fatalf("stage stamped binary: %v", err)
	}
	return dir
}

func TestUpdateHelpListsCommand(t *testing.T) {
	home := t.TempDir()
	binDir := freshInstalledBinary(t)
	// --help must work without touching the network.
	code, stdout, _ := runViaPath(t, binDir, home, "http://127.0.0.1:1", "update", "--help")
	if code != 0 {
		t.Fatalf("update --help exit = %d", code)
	}
	if !strings.Contains(stdout, "--check") {
		t.Fatalf("update --help should document --check:\n%s", stdout)
	}

	code, stdout, _ = runViaPath(t, binDir, home, "http://127.0.0.1:1", "--help")
	if code != 0 {
		t.Fatalf("--help exit = %d", code)
	}
	if !strings.Contains(stdout, "update") {
		t.Fatalf("root help should list the update command:\n%s", stdout)
	}
}

func TestUpdateCheckUpToDate(t *testing.T) {
	home := t.TempDir()
	binDir := freshInstalledBinary(t)
	srv := releaseTestServer(t, "1.0.0", nil, "")

	code, stdout, stderr := runViaPath(t, binDir, home, srv.URL, "update", "--check")
	if code != 0 {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "already up to date (v1.0.0)") {
		t.Fatalf("expected up-to-date message, got:\n%s", stdout)
	}
}

func TestUpdateCheckAvailable(t *testing.T) {
	home := t.TempDir()
	binDir := freshInstalledBinary(t)
	srv := releaseTestServer(t, "1.0.1", readBytes(t, releasePayload), "")

	code, stdout, stderr := runViaPath(t, binDir, home, srv.URL, "update", "--check")
	if code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s", code, stderr)
	}
	for _, want := range []string{"Current version: v1.0.0", "Latest version:", "Update available."} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("expected %q in output:\n%s", want, stdout)
		}
	}
	// --check must leave the binary alone: a follow-up run still reports 1.0.0.
	code, stdout, _ = runViaPath(t, binDir, home, srv.URL, "version", "--short")
	if code != 0 || strings.TrimSpace(stdout) != "1.0.0" {
		t.Fatalf("--check modified the binary: exit %d, version %q", code, stdout)
	}
}

func TestUpdateFullReplacement(t *testing.T) {
	home := t.TempDir()
	binDir := freshInstalledBinary(t)
	payload := readBytes(t, releasePayload)
	srv := releaseTestServer(t, "1.0.1", payload, sha256HexOf(t, payload))

	code, stdout, stderr := runViaPath(t, binDir, home, srv.URL, "update")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Phelix updated successfully: v1.0.0 → v1.0.1") {
		t.Fatalf("missing success line:\n%s", stdout)
	}

	// The replaced binary must be the new release: `phelix version` reports
	// it from the same version source the updater compared against.
	code, stdout, _ = runViaPath(t, binDir, home, srv.URL, "version", "--short")
	if code != 0 || strings.TrimSpace(stdout) != "1.0.1" {
		t.Fatalf("updated binary reports %q (exit %d), want 1.0.1", stdout, code)
	}
}

func TestUpdateChecksumMismatchKeepsOldBinary(t *testing.T) {
	home := t.TempDir()
	binDir := freshInstalledBinary(t)
	srv := releaseTestServer(t, "1.0.1", readBytes(t, releasePayload), strings.Repeat("0", 64))

	code, stdout, stderr := runViaPath(t, binDir, home, srv.URL, "update")
	if code != 1 { // UPDATE_FAILED → ExitFailure
		t.Fatalf("exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "checksum mismatch") {
		t.Fatalf("stderr should report the mismatch:\n%s", stderr)
	}
	// The old binary must still work and report the old version.
	code, stdout, _ = runViaPath(t, binDir, home, srv.URL, "version", "--short")
	if code != 0 || strings.TrimSpace(stdout) != "1.0.0" {
		t.Fatalf("old binary damaged: exit %d, version %q", code, stdout)
	}
}

func TestUpdateUnversionedBinaryRefused(t *testing.T) {
	home := t.TempDir()
	// A binary built without ldflags reports version 0.0.0-dev, which the
	// updater must refuse to second-guess.
	binDir := buildUnversionedBinaryDir(t)
	srv := releaseTestServer(t, "1.0.1", nil, "")

	code, _, stderr := runViaPath(t, binDir, home, srv.URL, "update")
	if code != 40 { // CONFIGURATION_ERROR → ExitConfig
		t.Fatalf("exit = %d, want 40\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "could not be determined") {
		t.Fatalf("stderr should explain the refusal:\n%s", stderr)
	}
}

// buildUnversionedBinaryDir builds the CLI without version ldflags into a
// fresh directory (its "phelix" reports 0.0.0-dev) and returns the directory.
func buildUnversionedBinaryDir(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	dir := t.TempDir()
	cmd, err := testCmd("go", "build", "-o", filepath.Join(dir, "phelix"), ".")
	if err != nil {
		t.Fatalf("testCmd: %v", err)
	}
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	return dir
}

func TestUpdateUnreachableServer(t *testing.T) {
	home := t.TempDir()
	binDir := freshInstalledBinary(t)

	code, _, stderr := runViaPath(t, binDir, home, "http://127.0.0.1:1", "update")
	if code != 30 { // CONNECTION_ERROR → ExitNetwork
		t.Fatalf("exit = %d, want 30\nstderr:\n%s", code, stderr)
	}
}

// --- small helpers -------------------------------------------------------------

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func sha256HexOf(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
