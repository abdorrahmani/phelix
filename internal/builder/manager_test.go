package builder

import (
	"path/filepath"
	"testing"
)

func TestBuildManager_DetectLanguage(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  Language
	}{
		{"cargo at root", map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n"}, Rust},
		{"go.mod at root", map[string]string{"go.mod": "module x\n"}, Go},
		{"main.go at root", map[string]string{"main.go": "package main\n"}, Go},
		{"main pkg in subdir", map[string]string{"cmd/main.go": "package main\n"}, Go},
		{"nothing recognisable", map[string]string{"README.md": "hi\n"}, Language("unknown")},
		{"cargo wins over go.mod", map[string]string{
			"Cargo.toml": "[package]\nname = \"x\"\n", "go.mod": "module x\n"}, Rust},
		{"cargo wins over main.go", map[string]string{
			"Cargo.toml": "[package]\nname = \"x\"\n", "main.go": "package main\n"}, Rust},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for p, c := range tc.files {
				writeTestFile(t, filepath.Join(root, filepath.FromSlash(p)), c)
			}
			if got := NewBuildManager().DetectLanguage(root); got != tc.want {
				t.Fatalf("DetectLanguage = %q, want %q", got, tc.want)
			}
		})
	}
}

// A main package reachable only through vendor/ must not be detected.
func TestBuildManager_DetectLanguageIgnoresVendor(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "vendor", "dep", "main.go"), "package main\n")

	if got := NewBuildManager().DetectLanguage(root); got.IsSupported() {
		t.Fatalf("a main package under vendor must not be detected, got %q", got)
	}
}

// The core claim of this commit: the same root is walked once.
func TestBuildManager_ScanIsCachedPerRoot(t *testing.T) {
	bm := NewBuildManager()
	dir, other := t.TempDir(), t.TempDir()

	if a, b := bm.scanProject(dir), bm.scanProject(dir); a != b {
		t.Fatal("the same root must reuse the cached scan")
	}
	if bm.scanProject(other) == bm.scanProject(dir) {
		t.Fatal("a different root must trigger a new scan")
	}
}

// PrepareBuild must attach the same scan it validated with, so Build reuses
// it instead of walking again.
func TestPrepareBuild_SharesOneScanWithBuild(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module x\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n")

	bm := NewBuildManager()
	bc, err := bm.PrepareBuild("id", "app", root, filepath.Join(root, "out"), "", nil, true)
	if err != nil {
		t.Fatalf("PrepareBuild: %v", err)
	}
	if bc.Config.Scan == nil {
		t.Fatal("PrepareBuild must attach the shared scan to the build config")
	}
	if bc.Config.Scan != bm.scanProject(root) {
		t.Fatal("PrepareBuild must reuse the manager's cached scan")
	}
}
