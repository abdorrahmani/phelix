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
