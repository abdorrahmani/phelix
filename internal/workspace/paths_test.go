package workspace

import (
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestSanitizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"my-mesh", "my-mesh"},
		{"My_Mesh.1", "My_Mesh.1"},
		{"a/b c", "a-b-c"},
		{"  spaced  ", "spaced"},
		{"weird***name", "weird-name"},
		{"--trim--", "trim"},
		{"a///b", "a-b"},
		{"", "workspace"},
		{"...", "..."},
		{".", "workspace"},
		{"..", "workspace"},
		{"/", "workspace"},
	}
	for _, tt := range tests {
		if got := SanitizeName(tt.in); got != tt.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWorkspacePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wantDir := filepath.Join(home, ".phelix", "workspaces", "my-mesh")

	dir, err := WorkspaceDir("my-mesh")
	if err != nil || dir != wantDir {
		t.Fatalf("WorkspaceDir = %q, %v; want %q", dir, err, wantDir)
	}
	for _, c := range []struct {
		fn   func(string) (string, error)
		file string
	}{
		{StatePath, "workspace.json"},
		{LockPath, "workspace.lock"},
		{LogPath, "workspace.log"},
	} {
		got, err := c.fn("my-mesh")
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if want := filepath.Join(wantDir, c.file); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestEnsureWorkspaceDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := EnsureWorkspaceDir("mesh")
	if err != nil {
		t.Fatalf("EnsureWorkspaceDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("perm = %o, want 700", perm)
	}
}

func TestFindManifest(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ManifestFileName), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := FindManifest(child)
	if err != nil {
		t.Fatalf("FindManifest: %v", err)
	}
	if want := filepath.Join(root, ManifestFileName); got != want {
		t.Errorf("FindManifest = %q, want %q", got, want)
	}

	// No manifest anywhere up from an isolated empty dir → NOT_FOUND.
	if _, err := FindManifest(t.TempDir()); phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Errorf("code = %q, want NOT_FOUND", phelixerr.CodeOf(err))
	}
}
