package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectLanguage_GoProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/foo\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lang, err := DetectLanguage(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lang != LanguageGo {
		t.Errorf("expected LanguageGo, got %v", lang)
	}
}

func TestDetectLanguage_RustProject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"foo\"\nversion = \"0.1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lang, err := DetectLanguage(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lang != LanguageRust {
		t.Errorf("expected LanguageRust, got %v", lang)
	}
}

func TestDetectLanguage_MissingBoth(t *testing.T) {
	dir := t.TempDir()

	_, err := DetectLanguage(dir)
	if err == nil {
		t.Fatal("expected error for missing project files")
	}
	if err.Error() != "unsupported project: neither go.mod nor Cargo.toml found in "+dir {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestDetectLanguage_Ambiguous(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/foo\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"foo\"\nversion = \"0.1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := DetectLanguage(dir)
	if err == nil {
		t.Fatal("expected error for ambiguous project")
	}
	if err.Error() != "ambiguous project: both go.mod and Cargo.toml found in "+dir {
		t.Errorf("unexpected error message: %v", err)
	}
}
