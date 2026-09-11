package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultDockerignore_ExcludesSecrets(t *testing.T) {
	content := DefaultDockerignore()

	// Must exclude environment files (potential secrets)
	for _, pattern := range []string{".env", ".env.*", "*.env"} {
		if !strings.Contains(content, pattern) {
			t.Errorf("dockerignore missing secret-exclusion pattern: %s", pattern)
		}
	}

	// Must exclude VCS data
	if !strings.Contains(content, ".git") {
		t.Error("dockerignore should exclude .git")
	}

	// Must exclude build artifacts
	if !strings.Contains(content, "app_*") {
		t.Error("dockerignore should exclude app_* build artifacts")
	}

	// Must exclude Phelix internals
	if !strings.Contains(content, ".phelix/") {
		t.Error("dockerignore should exclude .phelix/")
	}
}

func TestHasExistingDockerignore(t *testing.T) {
	dir := t.TempDir()

	if HasExistingDockerignore(dir) {
		t.Error("should not find .dockerignore in empty directory")
	}

	if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !HasExistingDockerignore(dir) {
		t.Error("should find .dockerignore after creating one")
	}
}

func TestWriteDockerignore_NoOverwrite(t *testing.T) {
	dir := t.TempDir()
	existingContent := "# custom\ntarget/\n"
	if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte(existingContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should not overwrite existing file
	if err := WriteDockerignore(dir); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if string(data) != existingContent {
		t.Error("WriteDockerignore should not overwrite existing .dockerignore")
	}
}

func TestWriteDockerignore_CreatesNew(t *testing.T) {
	dir := t.TempDir()

	if err := WriteDockerignore(dir); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}

	if len(data) == 0 {
		t.Error("WriteDockerignore should create non-empty .dockerignore")
	}
}
