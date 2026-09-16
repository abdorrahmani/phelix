package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- `phelix watch` end-to-end (real binary, isolated HOME) -------------------
//
// These pin the user-visible contract of the watching flag: apps default to
// disabled (including state files written before the field existed), the
// toggle persists across processes, and list/status surface the state.

// seedWatchingApps writes a legacy-format apps.json (no "watching" key) with
// two apps, the exact shape existing installations upgrade from.
func seedWatchingApps(t *testing.T, home string) {
	t.Helper()
	phelixDir := filepath.Join(home, ".phelix")
	if err := os.MkdirAll(filepath.Join(phelixDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	appsJSON := `{
  "203": {
    "id": "203",
    "name": "api",
    "pid": 0,
    "status": "stopped",
    "port": 8080,
    "directory": "/tmp/api",
    "language": "go"
  },
  "204": {
    "id": "204",
    "name": "worker",
    "pid": 0,
    "status": "stopped",
    "port": 8081,
    "directory": "/tmp/worker",
    "language": "go"
  }
}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps.json"), []byte(appsJSON), 0o644); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}
}

func TestCLIWatch_Lifecycle(t *testing.T) {
	home := t.TempDir()
	seedWatchingApps(t, home)

	code, stdout, stderr := runPhelixInHome(t, home, "list")
	if code != 0 {
		t.Fatalf("phelix list exit = %d, want 0; stderr=%q", code, stderr)
	}
	for _, want := range []string{"WATCHING", "api", "worker"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("initial list missing %q:\n%s", want, stdout)
		}
	}
	if n := strings.Count(stdout, "disabled"); n < 2 {
		t.Fatalf("expected both legacy apps to show as disabled, found %d:\n%s", n, stdout)
	}
	if strings.Contains(stdout, "enabled") {
		t.Fatalf("no app may be enabled before `phelix watch`:\n%s", stdout)
	}

	// Enable watching for one app.
	code, stdout, stderr = runPhelixInHome(t, home, "watch", "api")
	if code != 0 {
		t.Fatalf("phelix watch api exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "enabled") {
		t.Fatalf("watch output missing confirmation:\n%s", stdout)
	}

	code, stdout, stderr = runPhelixInHome(t, home, "list")
	if code != 0 {
		t.Fatalf("phelix list exit = %d, want 0; stderr=%q", code, stderr)
	}
	if n := strings.Count(stdout, "enabled"); n != 1 {
		t.Fatalf("expected exactly one enabled app, found %d:\n%s", n, stdout)
	}
	if n := strings.Count(stdout, "disabled"); n != 1 {
		t.Fatalf("expected exactly one disabled app, found %d:\n%s", n, stdout)
	}

	code, stdout, stderr = runPhelixInHome(t, home, "status", "api")
	if code != 0 {
		t.Fatalf("phelix status api exit = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "WATCHING") || !strings.Contains(stdout, "enabled") {
		t.Fatalf("status for watched app must show WATCHING enabled:\n%s", stdout)
	}

	code, stdout, stderr = runPhelixInHome(t, home, "status", "worker")
	if code != 0 {
		t.Fatalf("phelix status worker exit = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "disabled") {
		t.Fatalf("status for unwatched app must show disabled:\n%s", stdout)
	}

	code, stdout, stderr = runPhelixInHome(t, home, "watch", "api", "--disable")
	if code != 0 {
		t.Fatalf("phelix watch api --disable exit = %d, want 0; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "disabled") {
		t.Fatalf("watch --disable output missing confirmation:\n%s", stdout)
	}

	code, stdout, _ = runPhelixInHome(t, home, "status", "api")
	if code != 0 || !strings.Contains(stdout, "disabled") {
		t.Fatalf("status after disable must show disabled (code=%d):\n%s", code, stdout)
	}
}

func TestCLIWatch_AppNotFound(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatalf("mkdir .phelix: %v", err)
	}

	code, stdout, stderr := runPhelixInHome(t, home, "watch", "missing-app")
	if code != ExitNotFound {
		t.Fatalf("phelix watch missing-app exit = %d, want %d (NOT_FOUND); stdout=%q stderr=%q",
			code, ExitNotFound, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout polluted with error output: %q", stdout)
	}
}

func TestCLIWatch_ResolvesByName(t *testing.T) {
	home := t.TempDir()
	seedWatchingApps(t, home)

	code, _, stderr := runPhelixInHome(t, home, "watch", "worker")
	if code != 0 {
		t.Fatalf("phelix watch worker exit = %d, want 0; stderr=%q", code, stderr)
	}

	code, stdout, _ := runPhelixInHome(t, home, "status", "worker")
	if code != 0 || !strings.Contains(stdout, "enabled") {
		t.Fatalf("status after watch-by-name must show enabled (code=%d):\n%s", code, stdout)
	}
}

// seedWatchYamlApps writes an apps.json whose apps point at real project
// directories: api's has an init-style phelix.yaml (watching: disable),
// worker's has none.
func seedWatchYamlApps(t *testing.T, home, apiDir, workerDir string) {
	t.Helper()
	phelixDir := filepath.Join(home, ".phelix")
	if err := os.MkdirAll(filepath.Join(phelixDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	appsJSON := `{
  "301": {
    "id": "301",
    "name": "api",
    "pid": 0,
    "status": "stopped",
    "port": 8080,
    "directory": "` + apiDir + `",
    "language": "go",
    "watching": false
  },
  "302": {
    "id": "302",
    "name": "worker",
    "pid": 0,
    "status": "stopped",
    "port": 8081,
    "directory": "` + workerDir + `",
    "language": "go",
    "watching": false
  }
}`
	if err := os.WriteFile(filepath.Join(phelixDir, "apps.json"), []byte(appsJSON), 0o644); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}
}

// `phelix watch` must keep the two copies of the watching state from
// diverging: the runtime flag in apps.json AND the watching: key in the
// project's phelix.yaml (the desired state the next build/rebuild converges
// on — otherwise a rebuild would silently revert the toggle).
func TestCLIWatch_UpdatesProjectYaml(t *testing.T) {
	home := t.TempDir()
	apiDir := t.TempDir()
	workerDir := t.TempDir()

	const initStyle = "# Phelix project configuration.\nname: api\nport: 3000\nwatching: disable\n"
	if err := os.WriteFile(filepath.Join(apiDir, "phelix.yaml"), []byte(initStyle), 0o644); err != nil {
		t.Fatalf("write phelix.yaml: %v", err)
	}
	seedWatchYamlApps(t, home, apiDir, workerDir)

	readYaml := func() string {
		raw, err := os.ReadFile(filepath.Join(apiDir, "phelix.yaml"))
		if err != nil {
			t.Fatalf("read phelix.yaml: %v", err)
		}
		return string(raw)
	}

	// Enable: both the runtime flag and the yaml key flip.
	code, stdout, stderr := runPhelixInHome(t, home, "watch", "api")
	if code != 0 {
		t.Fatalf("phelix watch api exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "Updated watching: enable") {
		t.Fatalf("watch output missing the yaml-update note:\n%s", stdout)
	}
	yaml := readYaml()
	for _, want := range []string{"watching: enable", "name: api", "port: 3000", "# Phelix project configuration."} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("phelix.yaml after enable missing %q:\n%s", want, yaml)
		}
	}

	// Disable: both flip back.
	code, stdout, stderr = runPhelixInHome(t, home, "watch", "api", "--disable")
	if code != 0 {
		t.Fatalf("phelix watch api --disable exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(readYaml(), "watching: disable") {
		t.Fatalf("phelix.yaml after --disable:\n%s", readYaml())
	}

	// An app whose project has no phelix.yaml: the toggle still works, no
	// file is created, and the note says only the runtime state changed.
	code, stdout, stderr = runPhelixInHome(t, home, "watch", "worker")
	if code != 0 {
		t.Fatalf("phelix watch worker exit = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "only the runtime state was changed") {
		t.Fatalf("watch output missing the no-yaml note:\n%s", stdout)
	}
	if _, serr := os.Stat(filepath.Join(workerDir, "phelix.yaml")); !os.IsNotExist(serr) {
		t.Fatal("phelix watch created a phelix.yaml; that is phelix init's job")
	}

	code, stdout, _ = runPhelixInHome(t, home, "status", "worker")
	if code != 0 || !strings.Contains(stdout, "enabled") {
		t.Fatalf("worker must be enabled in status (code=%d):\n%s", code, stdout)
	}
}
