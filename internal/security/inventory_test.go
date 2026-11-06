package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectEcosystem(t *testing.T) {
	goDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if eco, err := DetectEcosystem(goDir); err != nil || eco != EcosystemGo {
		t.Fatalf("go.mod dir: eco=%s err=%v", eco, err)
	}

	rustDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(rustDir, "Cargo.toml"), []byte("[package]\nname=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if eco, err := DetectEcosystem(rustDir); err != nil || eco != EcosystemRust {
		t.Fatalf("Cargo.toml dir: eco=%s err=%v", eco, err)
	}

	bothDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bothDir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bothDir, "Cargo.toml"), []byte("[package]\nname=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectEcosystem(bothDir); err == nil {
		t.Fatal("mixed markers must be ambiguous")
	}

	emptyDir := t.TempDir()
	if _, err := DetectEcosystem(emptyDir); err == nil {
		t.Fatal("no markers must fail")
	}
}
