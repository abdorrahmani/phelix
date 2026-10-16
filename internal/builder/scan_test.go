package builder

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestFile creates a file at the specified path with the given content, creating any necessary parent directories. If any operation fails, it calls t.Fatalf to fail the test.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestProjectScan_SkipsIgnoredDirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"vendor", "node_modules", "target", ".git"} {
		writeTestFile(t, filepath.Join(root, dir, "main.go"), "package main\n")
	}

	s := NewProjectScan(root)
	if s.HasMainFile() {
		t.Fatalf("main.go inside an ignored dir must not be found, got %q", s.FirstMainFile())
	}
	if s.HasMainPackage() {
		t.Fatal("package main inside an ignored dir must not be detected")
	}
}

func TestProjectScan_FindsMainFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "cmd", "app", "main.go"), "package main\n")

	s := NewProjectScan(root)
	if !s.HasMainFile() {
		t.Fatal("expected main.go under cmd/app to be found")
	}
	if want := filepath.Join(root, "cmd", "app", "main.go"); s.FirstMainFile() != want {
		t.Fatalf("FirstMainFile = %q, want %q", s.FirstMainFile(), want)
	}
}

func TestProjectScan_RootFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module x\n")

	s := NewProjectScan(root)
	if !s.RootFile("go.mod") {
		t.Fatal("go.mod at root must be reported")
	}
	if s.RootFile("Cargo.toml") {
		t.Fatal("absent Cargo.toml must not be reported")
	}
}

func TestProjectScan_MissingRoot(t *testing.T) {
	s := NewProjectScan(filepath.Join(t.TempDir(), "nope"))
	if s.Exists {
		t.Fatal("a missing root must report Exists=false")
	}
	if s.HasMainFile() || s.RootFile("go.mod") {
		t.Fatal("a missing root must yield no findings")
	}
}

// The root must be scanned even when its own name matches an ignored dir.
func TestProjectScan_RootNamedLikeIgnoredDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vendor")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n")

	if !NewProjectScan(root).HasMainFile() {
		t.Fatal("the root must never be skipped, even when named like an ignored dir")
	}
}

func TestProjectScan_DeterministicOrder(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "b", "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, "a", "main.go"), "package main\n")

	first := NewProjectScan(root).FirstMainFile()
	for i := 0; i < 5; i++ {
		if got := NewProjectScan(root).FirstMainFile(); got != first {
			t.Fatalf("walk order must be stable: %q vs %q", got, first)
		}
	}
	if want := filepath.Join(root, "a", "main.go"); first != want {
		t.Fatalf("expected the lexically first main.go %q, got %q", want, first)
	}
}

func TestProjectScan_HasMainPackage(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pkg", "lib.go"), "package lib\n")
	if NewProjectScan(root).HasMainPackage() {
		t.Fatal("package lib must not count as package main")
	}

	writeTestFile(t, filepath.Join(root, "cmd", "tool.go"), "package main\n")
	if !NewProjectScan(root).HasMainPackage() {
		t.Fatal("package main in a subdir must be detected")
	}
}

func TestProjectScan_NilSafe(t *testing.T) {
	var s *ProjectScan
	if s.HasMainFile() || s.HasMainPackage() || s.RootFile("go.mod") || s.FirstMainFile() != "" {
		t.Fatal("a nil scan must be safe and report nothing")
	}
}
