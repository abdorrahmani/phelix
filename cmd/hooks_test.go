package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/project"
)

func TestHooksForAppReadsConfiguredHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	body := "name: hooked\nport: 8080\nhooks:\n  pre_deploy:\n    command: [\"./migrate\", \"up\"]\n    timeout: 2m\n  post_deploy:\n    command: [\"./notify\"]\n"
	if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, store, err := hooksForApp(&app.AppInfo{ID: "id-1", Name: "hooked", Directory: dir})
	if err != nil {
		t.Fatalf("hooksForApp: %v", err)
	}
	if got := cfg.Hooks.Configured(); len(got) != 2 || got[0] != project.HookPreDeploy || got[1] != project.HookPostDeploy {
		t.Fatalf("Configured() = %v", got)
	}
	hookDir, err := store.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".phelix", "apps", "hooked", "hooks"); hookDir != want {
		t.Errorf("store dir = %s, want %s", hookDir, want)
	}
}

func TestHooksForAppWithoutProjectConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, _, err := hooksForApp(&app.AppInfo{Name: "plain", Directory: t.TempDir()})
	if err != nil {
		t.Fatalf("hooksForApp: %v", err)
	}
	if cfg != nil {
		t.Fatalf("cfg = %+v, want nil for a directory without phelix.yaml", cfg)
	}
}

func TestHooksInspectRejectsMissingExecution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, store, err := hooksForApp(&app.AppInfo{Name: "missing", Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("does-not-exist"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Load error = %v, want not found", err)
	}
}
