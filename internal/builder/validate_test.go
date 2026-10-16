package builder

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGoBuilderValidate(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module x\n")

	if err := NewGoBuilder().Validate(NewProjectScan(root)); err != nil {
		t.Fatalf("go.mod at root must validate: %v", err)
	}
}

func TestGoBuilderValidate_MainFileInSubdir(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "cmd", "main.go"), "package main\n")

	if err := NewGoBuilder().Validate(NewProjectScan(root)); err != nil {
		t.Fatalf("main.go in a subdir must validate: %v", err)
	}
}

func TestGoBuilderValidate_EmptyProject(t *testing.T) {
	err := NewGoBuilder().Validate(NewProjectScan(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "no valid Go project") {
		t.Fatalf("an empty project must be rejected, got %v", err)
	}
}

func TestGoBuilderValidate_MissingRoot(t *testing.T) {
	err := NewGoBuilder().Validate(NewProjectScan(filepath.Join(t.TempDir(), "nope")))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a missing root must be NOT_FOUND, got %v", err)
	}
}

func TestGoBuilderValidate_NilScan(t *testing.T) {
	if err := NewGoBuilder().Validate(nil); err == nil {
		t.Fatal("a nil scan must be rejected")
	}
}

// A main.go only under vendor/ must not make the project valid.
func TestGoBuilderValidate_IgnoresVendor(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "vendor", "dep", "main.go"), "package main\n")

	if err := NewGoBuilder().Validate(NewProjectScan(root)); err == nil {
		t.Fatal("a main.go under vendor must not validate the project")
	}
}

func TestRustBuilderValidate(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "Cargo.toml"), "[package]\nname = \"x\"\n")

	if err := NewRustBuilder().Validate(NewProjectScan(root)); err != nil {
		t.Fatalf("Cargo.toml at root must validate: %v", err)
	}
	if err := NewRustBuilder().Validate(NewProjectScan(t.TempDir())); err == nil {
		t.Fatal("a project without Cargo.toml must be rejected")
	}
	if err := NewRustBuilder().Validate(nil); err == nil {
		t.Fatal("a nil scan must be rejected")
	}
}
