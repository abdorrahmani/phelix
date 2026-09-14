package cmd

// watching_yaml_integration_test.go pins the phelix.yaml side of the watching
// flag end-to-end: `phelix init` writes the disable default, and `phelix
// build` applies the project's declared value to the app entry.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/project"
)

// runPhelixIn runs the real CLI in dir with an isolated HOME and data dir.
func runPhelixIn(t *testing.T, home, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(phelixBin(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PHELIX_DATA_DIR="+filepath.Join(home, ".phelix"),
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
			t.Fatalf("failed to run phelix %v: %v", args, err)
		}
	}
	return code, stdout.String(), stderr.String()
}

// `phelix init` must generate a phelix.yaml whose watching key defaults to
// disable, in a shape project.Load validates.
func TestCLIInit_WatchingDefaultsToDisable(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/initapp\n\ngo 1.22\n",
		"main.go": goPORTMain,
	})
	home := t.TempDir()

	code, _, stderr := runPhelixIn(t, home, dir, "init", "--yes")
	if code != 0 {
		t.Fatalf("phelix init exit = %d, want 0; stderr=%q", code, stderr)
	}

	raw, err := os.ReadFile(filepath.Join(dir, project.FileName))
	if err != nil {
		t.Fatalf("read %s: %v", project.FileName, err)
	}
	if !strings.Contains(string(raw), "watching: disable") {
		t.Fatalf("%s missing the disable default:\n%s", project.FileName, raw)
	}

	cfg, err := project.Load(dir)
	if err != nil {
		t.Fatalf("generated %s does not load: %v", project.FileName, err)
	}
	if enabled, ok := cfg.WatchingSetting(); !ok || enabled {
		t.Fatalf("generated watching = (%v, present=%v), want (false, true)", enabled, ok)
	}
}

// `phelix build` applies the watching value declared in phelix.yaml to the
// app entry: enable makes the app watched; an absent key leaves the disabled
// default. The state is what a later process (status/list, the monitor
// daemon) reads.
func TestCLIBuild_AppliesWatchingFromProjectConfig(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		wantEnabled bool
	}{
		{"enable", "name: watchon\nport: 48991\nwatching: enable\n", true},
		{"absent", "name: watchoff\nport: 48992\n", false},
		{"disable", "name: watchno\nport: 48993\nwatching: disable\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeProject(t, map[string]string{
				"go.mod":      "module example.com/" + tc.name + "\n\ngo 1.22\n",
				"main.go":     goPORTMain,
				"phelix.yaml": tc.yaml,
			})
			home := t.TempDir()

			code, _, stderr := runPhelixIn(t, home, dir, "build")
			if code != 0 {
				t.Fatalf("phelix build exit = %d, want 0; stderr=%q", code, stderr)
			}

			// A fresh process must see the applied state.
			appName := strings.Fields(tc.yaml)[1] // first token after "name:"
			code, stdout, stderr := runPhelixIn(t, home, dir, "status", appName)
			if code != 0 {
				t.Fatalf("phelix status exit = %d, want 0; stderr=%q", code, stderr)
			}
			want := "disabled"
			if tc.wantEnabled {
				want = "enabled"
			}
			if !strings.Contains(stdout, "WATCHING") || !strings.Contains(stdout, want) {
				t.Fatalf("status output missing WATCHING %s:\n%s", want, stdout)
			}

			// The build leaves the app running; stop it so the test leaks no
			// listener.
			if code, _, stderr := runPhelixIn(t, home, dir, "stop", appName); code != 0 {
				t.Fatalf("phelix stop exit = %d, want 0; stderr=%q", code, stderr)
			}
		})
	}
}
