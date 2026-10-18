package cmd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const brokenGoMain = `package main

func main() {
	undefinedSymbol()
}
`

func TestCLIBuild_FailedBuildFreesTheName(t *testing.T) {
	home := t.TempDir()
	phelixDir := filepath.Join(home, ".phelix")
	if err := os.MkdirAll(filepath.Join(phelixDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/broken\n\ngo 1.22\n",
		"main.go": brokenGoMain,
	})
	port := strconv.Itoa(freePortForTest(t))

	code, _, stderr := runPhelixIn(t, home, dir, "build", "burned", "--port", port)
	if code != ExitBuild {
		t.Fatalf("build exit = %d, want %d (BUILD_FAILED); stderr=%q", code, ExitBuild, stderr)
	}

	code, _, stderr = runPhelixIn(t, home, dir, "build", "burned", "--port", port)
	if code != ExitBuild {
		t.Fatalf("second build exit = %d, want %d; stderr=%q", code, ExitBuild, stderr)
	}
	if strings.Contains(stderr, "already in use") {
		t.Fatalf("the failed build burned the name: %q", stderr)
	}

	raw, err := os.ReadFile(filepath.Join(phelixDir, "apps.json"))
	if err != nil {
		t.Fatalf("read apps.json: %v", err)
	}
	if strings.Contains(string(raw), "burned") {
		t.Fatalf("a failed build left an entry behind: %s", raw)
	}
}

// A name that really is taken keeps failing — and now names the way out.
func TestCLIBuild_TakenNameNamesTheRemoveCommand(t *testing.T) {
	home := t.TempDir()
	phelixDir := filepath.Join(home, ".phelix")
	if err := os.MkdirAll(filepath.Join(phelixDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	appsJSON := `{
  "203": {
    "id": "203",
    "name": "taken",
    "pid": 0,
    "status": "stopped",
    "build_status": "built"
  }
}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps.json"), []byte(appsJSON), 0o644); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/taken\n\ngo 1.22\n",
		"main.go": goPORTMain,
	})

	code, _, stderr := runPhelixIn(t, home, dir, "build", "taken", "--port", strconv.Itoa(freePortForTest(t)))
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitFailure, stderr)
	}
	if !strings.Contains(stderr, "phelix remove") {
		t.Fatalf("duplicate-name error does not name the way out: %q", stderr)
	}
}

// freePortForTest returns a port that is free at the moment of the call. The
// listener is closed before the CLI starts, so a race is possible in principle
// and beside the point here — the build reserves its port for the whole
// compile, which is exactly the behaviour these tests must not fight.
func freePortForTest(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
