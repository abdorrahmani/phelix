package controller

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledClientEndpointAndExplicitOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"PHELIX_CONTROLLER_CONFIG", "PHELIX_CONTROL_SOCKET", "PHELIX_DATA_DIR"} {
		t.Setenv(name, "")
	}
	installed := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(installed, []byte("socket: /run/phelix/control.sock\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadClientConfig(installed)
	if err != nil || cfg.Socket != "/run/phelix/control.sock" {
		t.Fatal(cfg, err)
	}
	t.Setenv("PHELIX_CONTROL_SOCKET", "/explicit/control.sock")
	cfg, err = loadClientConfig(installed)
	if err != nil || cfg.Socket != "/explicit/control.sock" {
		t.Fatal(cfg, err)
	}
	t.Setenv("PHELIX_CONTROL_SOCKET", "")
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	cfg, err = loadClientConfig(installed)
	if err != nil || cfg.Socket != filepath.Join(os.Getenv("PHELIX_DATA_DIR"), "control.sock") {
		t.Fatal("explicit state directory was overridden", cfg, err)
	}
}
