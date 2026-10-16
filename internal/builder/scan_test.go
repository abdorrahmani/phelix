package builder

import (
	"os"
	"path/filepath"
	"strings"
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

// One entry per sub-case so each form is proven independently. A single
// .gitignore listing several entries passes whenever any one of them works —
// which is how a parser that silently rejected every "dir/" form went
// unnoticed.
func TestProjectScan_HonoursGitignoreDirs(t *testing.T) {
	for _, entry := range []string{"dist", "dist/", "coverage", "coverage/"} {
		t.Run(entry, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, ".gitignore"), entry+"\n")
			// Only the ignored dir exists, so a found main.go can only come
			// from that dir not being skipped.
			dir := strings.TrimSuffix(entry, "/")
			writeTestFile(t, filepath.Join(root, dir, "main.go"), "package main\n")

			if s := NewProjectScan(root); s.HasMainFile() {
				t.Fatalf("entry %q must be skipped, got %q", entry, s.FirstMainFile())
			}
		})
	}
}

// A comment, a negation, a glob and an embedded path in one file: only the
// bare names among them may skip anything.
func TestProjectScan_GitignoreMixedEntries(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".gitignore"),
		"# build output\ndist/\ncoverage\n!keepme\n*.log\nsrc/generated\n\n")
	writeTestFile(t, filepath.Join(root, "dist", "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, "coverage", "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, "keepme", "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, "src", "generated", "main.go"), "package main\n")

	s := NewProjectScan(root)
	// keepme (negated) and src/generated (embedded path) must still be walked.
	if len(s.mainFiles) != 2 {
		t.Fatalf("expected the 2 non-bare-name dirs to survive, got %d: %v", len(s.mainFiles), s.mainFiles)
	}
}

// Globs, negations and paths are not bare directory names and must not be
// applied as if they were.
func TestProjectScan_GitignoreOnlyBareDirNames(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".gitignore"), "*.tmp\n!important\nsrc/gen\n")
	writeTestFile(t, filepath.Join(root, "important", "main.go"), "package main\n")

	if !NewProjectScan(root).HasMainFile() {
		t.Fatal("a negated or glob pattern must not cause a directory to be skipped")
	}
}

func TestProjectScan_MissingGitignoreIsFine(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n")

	if !NewProjectScan(root).HasMainFile() {
		t.Fatal("a project without .gitignore must still scan normally")
	}
}

func TestProjectScan_UnreadableDirDoesNotAbortWalk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "sub", "main.go"), "package main\n")
	os.MkdirAll(filepath.Join(root, "aaa_locked"), 0o755)
	os.Chmod(filepath.Join(root, "aaa_locked"), 0o000)
	defer os.Chmod(filepath.Join(root, "aaa_locked"), 0o755)

	if s := NewProjectScan(root); s.FirstMainFile() == "" {
		t.Fatalf("unreadable dir aborted the walk: %v", s.mainFiles)
	}
}
