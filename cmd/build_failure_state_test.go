package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
)

func TestBuildApplication_FailureMarksEntryFailed(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name: "compile failure",
			setup: func(t *testing.T, dir string) {
				writeFixtureProject(t, dir,
					"module example.com/broken\n\ngo 1.22\n",
					"package main\n\nfunc main() {\n\tundefinedSymbol()\n\t\t// No go.mod and no main.go: PrepareBuild rejects the project\n\t\t\t\t// before the compile is ever reached.}\n")
			},
		},
		{
			name: "preparation failure",
			setup: func(t *testing.T, dir string) {
				// No go.mod and no main.go: PrepareBuild rejects the project
				// before to compile is ever reached.
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := withAppStore(t)
			info.Status = "initializing"
			info.BuildStatus = "building"

			dir := t.TempDir()
			info.Directory = dir
			tc.setup(t, dir)

			bin := filepath.Join(dir, "app_"+info.ID)
			var buildErr error
			captureStdout(t, func() {
				_, buildErr = buildApplication(info.ID, nil, builder.NewBuildManager(), bin)
			})
			if buildErr == nil {
				t.Fatal("building a broken project reported success")
			}

			got := app.Manager.(*app.AppManager).Apps[info.ID]
			if got == nil {
				t.Fatal("entry gone: a failed build must stay inspectable and removable")
			}
			if got.Status != "failed" {
				t.Fatalf("Status = %q, want %q: only BuildStatus was normalized before, so the app read as initializing forever",
					got.Status, "failed")
			}
			if got.BuildStatus != "failed" {
				t.Fatalf("BuildStatus = %q, want %q", got.BuildStatus, "failed")
			}
		})
	}
}

// writeFixtureProject writes a minimal Go project into dir.
func writeFixtureProject(t *testing.T, dir, goMod, mainGo string) {
	t.Helper()
	for name, body := range map[string]string{"go.mod": goMod, "main.go": mainGo} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}
