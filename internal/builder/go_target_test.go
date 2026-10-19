package builder

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGoBuildTarget(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "go.mod at root, main package at root",
			files: map[string]string{
				"go.mod":  "module example.com/app\n\ngo 1.22\n",
				"main.go": "package main\n\nfunc main() { helper() }\n",
				"util.go": "package main\n\nfunc helper() {}\n",
			},
			want: ".",
		},
		{
			name: "go.mod at root, main package in a subdirectory",
			files: map[string]string{
				"go.mod":          "module example.com/app\n\ngo 1.22\n",
				"cmd/app/main.go": "package main\n\nfunc main() {}\n",
			},
			want: "./cmd/app",
		},
		{
			name: "no go.mod, main.go at root",
			files: map[string]string{
				"main.go": "package main\n\nfunc main() {}\n",
				"util.go": "package main\n\nfunc helper() {}\n",
			},
			want: "main.go",
		},
		{
			name: "no go.mod, main package in a subdirectory",
			files: map[string]string{
				"sub/main.go": "package main\n\nfunc main() {}\n",
			},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for p, body := range tc.files {
				writeTestFile(t, filepath.Join(root, filepath.FromSlash(p)), body)
			}

			got, err := NewProjectScan(root).GoBuildTarget()
			if tc.want == "" {
				if err == nil {
					t.Fatalf("target = %q, want a refusal", got)
				}
				if !strings.Contains(err.Error(), "main.go sits in the project root") {
					t.Fatalf("refusal must say where a main.go has to live, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GoBuildTarget: %v", err)
			}
			if got != tc.want {
				t.Fatalf("target = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGoBuildTargetIsNeverAFileWithinAModule(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/app\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() { helper() }\n")
	writeTestFile(t, filepath.Join(root, "util.go"), "package main\n\nfunc helper() {}\n")

	got, err := NewProjectScan(root).GoBuildTarget()
	if err != nil {
		t.Fatal(err)
	}

	if strings.HasSuffix(got, ".go") {
		t.Fatalf("target = %q: a .go file builds a single-file ad-hoc package, so any helper in a sibling file is undefined", got)
	}
}

// TestGoBuildTargetPrefersRootMainFile pins the fix for silently building the
// wrong program. WalkDir is depth-first and lexicographic, so a main.go under
// any directory sorting before "main.go" (".claude/", "cmd/", "examples/") is
// collected first; FirstMainFile returns it, and before this fix the target
// was derived from its directory.
func TestGoBuildTargetPrefersRootMainFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/app\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n\nfunc main() {}\n")
	writeTestFile(t, filepath.Join(root, ".claude", "worktrees", "wt", "examples", "blue-green", "main.go"),
		"package main\n\nfunc main() {}\n")

	scan := NewProjectScan(root)
	if got := scan.FirstMainFile(); got == filepath.Join(root, "main.go") {
		t.Fatal("fixture is not exercising the bug: walk order already reaches the root main.go first")
	}

	got, err := scan.GoBuildTarget()
	if err != nil {
		t.Fatalf("GoBuildTarget: %v", err)
	}
	if got != "." {
		t.Fatalf("target = %q, want %q: the root main.go must win over one found earlier in walk order", got, ".")
	}
}

// TestGoBuildTargetRefusesAmbiguousMainFiles: with several candidates and none
// at the root, Phelix cannot know which program was meant, so it must refuse
// and name them rather than build whichever the walk happened to reach first.
func TestGoBuildTargetRefusesAmbiguousMainFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/app\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "cmd", "api", "main.go"), "package main\n\nfunc main() {}\n")
	writeTestFile(t, filepath.Join(root, "cmd", "worker", "main.go"), "package main\n\nfunc main() {}\n")

	got, err := NewProjectScan(root).GoBuildTarget()
	if err == nil {
		t.Fatalf("target = %q, want a refusal", got)
	}
	for _, want := range []string{"cmd/api/main.go", "cmd/worker/main.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must list %s, got: %v", want, err)
		}
	}
}

// TestGoBuildTargetSingleSubdirMainFile: one candidate in a subdirectory is
// unambiguous and must still build, module or not.
func TestGoBuildTargetSingleSubdirMainFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/app\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "cmd", "app", "main.go"), "package main\n\nfunc main() {}\n")

	got, err := NewProjectScan(root).GoBuildTarget()
	if err != nil {
		t.Fatalf("GoBuildTarget: %v", err)
	}
	if got != "./cmd/app" {
		t.Fatalf("target = %q, want %q", got, "./cmd/app")
	}
}
