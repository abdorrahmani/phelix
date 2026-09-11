package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Matrix builds must not be blocked by the classic build path's unique-name
// check: a matrix build compiles artifacts for an application (existing ones
// included) without registering an app entry or starting an instance. The
// check moved to the classic path only.
//
// The fixture seeds an app with the same name in the isolated HOME and runs
// `phelix build` in a project whose matrix profile fails plan validation (an
// exclude rule matching nothing). With the old ordering the command died at
// the uniqueness check ("already in use") before ever reaching the matrix
// path; now the plan error is the one surfaced.
func TestCLIBuild_MatrixSkipsUniqueNameCheck(t *testing.T) {
	home := t.TempDir()
	phelixDir := filepath.Join(home, ".phelix")
	if err := os.MkdirAll(filepath.Join(phelixDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	appsJSON := `{
  "203": {
    "id": "203",
    "name": "myapp",
    "pid": 0,
    "status": "stopped",
    "built_status": "built"
  }
}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps.json"), []byte(appsJSON), 0o644); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}

	// Project with a matrix profile whose exclude rule matches no combination:
	// the run must fail at plan validation — after the old uniqueness position.
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":  "module myapp\n\ngo 1.22\n",
		"main.go": "package main\n\nfunc main() {}\n",
		"phelix.yaml": `
name: myapp
matrix:
  enabled: true
  go:
    versions: ["1.26"]
  platforms: [linux/amd64]
  exclude:
    - go: "1.27"
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	cmd := exec.Command(phelixBin(t), "build")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PHELIX_DATA_DIR="+phelixDir,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("failed to run phelix build: %v", err)
		}
	}

	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (INVALID_ARGUMENT from plan validation); stdout=%q stderr=%q",
			code, ExitUsage, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "exclude rule") {
		t.Fatalf("expected the exclude-rule plan error, got: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "already in use") || strings.Contains(stderr.String(), "already in use") {
		t.Fatalf("matrix build must not be blocked by the unique-name check; stdout=%q stderr=%q",
			stdout.String(), stderr.String())
	}
}
