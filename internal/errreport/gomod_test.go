package errreport

import (
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestGoModuleResolverKnownConditions(t *testing.T) {
	cases := []struct {
		name         string
		output       string
		wantTitle    string
		wantCommand  string
		wantDocs     string
		wantSuggests []string // substrings that must appear in Suggestion
	}{
		{
			name:        "go.mod missing",
			output:      "go: go.mod file not found in current directory or any parent directory; see 'go help modules'",
			wantTitle:   "go.mod file not found",
			wantCommand: "go mod init",
			wantDocs:    "https://go.dev/ref/mod",
		},
		{
			name:        "malformed go.mod unknown directive",
			output:      "go: errors parsing go.mod:\ngo.mod:5: unknown directive: toolchainx",
			wantTitle:   "Invalid go.mod",
			wantCommand: "",
			wantDocs:    "https://go.dev/ref/mod",
		},
		{
			name:        "missing go.sum entry",
			output:      "go: example.com/app imports github.com/foo/bar: missing go.sum entry for module providing package github.com/foo/bar",
			wantTitle:   "Missing go.sum entry",
			wantCommand: "go mod tidy",
			wantDocs:    "https://go.dev/ref/mod",
		},
		{
			name:        "undeclared dependency",
			output:      "main.go:4:2: no required module provides package github.com/foo/bar; to add it:\n\tgo get github.com/foo/bar",
			wantTitle:   "Missing Go dependency",
			wantCommand: "go mod tidy",
			wantDocs:    "https://go.dev/ref/mod",
		},
		{
			name:        "cannot find module providing package",
			output:      "main.go:4:2: cannot find module providing package github.com/foo/bar",
			wantTitle:   "Missing Go dependency",
			wantCommand: "go mod tidy",
		},
		{
			name:        "checksum mismatch",
			output:      "verifying github.com/foo/bar@v1.0.0: checksum mismatch\n\tdownloaded: h1:aaa...\n\tgo.sum:     h1:bbb...",
			wantTitle:   "Module checksum mismatch",
			wantCommand: "go clean -modcache && go mod download",
		},
		{
			name:        "unknown revision",
			output:      "go: github.com/foo/bar@v9.9.9: invalid version: unknown revision v9.9.9",
			wantTitle:   "Invalid module version",
			wantCommand: "",
		},
		{
			name:        "goproxy off",
			output:      "go: module lookup disabled by GOPROXY=off",
			wantTitle:   "Module lookup disabled",
			wantCommand: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, ok := For(buildFail("go", tc.output))
			if !ok {
				t.Fatalf("known condition not recognized: %q", tc.output)
			}
			if rep.Title != tc.wantTitle {
				t.Fatalf("Title = %q, want %q", rep.Title, tc.wantTitle)
			}
			if rep.Command != tc.wantCommand {
				t.Fatalf("Command = %q, want %q", rep.Command, tc.wantCommand)
			}
			if tc.wantDocs != "" && rep.DocsURL != tc.wantDocs {
				t.Fatalf("DocsURL = %q, want %q", rep.DocsURL, tc.wantDocs)
			}
			for _, want := range tc.wantSuggests {
				if !strings.Contains(rep.Suggestion, want) {
					t.Fatalf("Suggestion %q missing %q", rep.Suggestion, want)
				}
			}
			if rep.Code != "BUILD_FAILED" {
				t.Fatalf("Code = %s, want BUILD_FAILED", rep.Code)
			}
		})
	}
}

func TestGoModuleResolverNoTidyForCompileErrors(t *testing.T) {
	// The explicit "do not blindly recommend go mod tidy" guard: ordinary
	// compiler diagnostics must stay unknown so the renderer shows the raw
	// output instead of an irrelevant fix.
	outputs := []string{
		"# example.com/app\ncmd/main.go:9:2: undefined: Foo",
		"# example.com/app\n./main.go:12:3: cannot use x (variable of type int) as string value",
		"# example.com/app\n./main.go:5:2: syntax error: unexpected newline, expecting { after init func body",
		"go: cannot find main module",
	}
	for _, out := range outputs {
		if _, ok := For(buildFail("go", out)); ok {
			t.Fatalf("compiler diagnostic misclassified as module error: %q", out)
		}
	}
}

func TestGoModuleResolverRequiresGoToolEvidence(t *testing.T) {
	// Same output, but carried by a cargo failure: no Go module report.
	if _, ok := For(buildFail("cargo", "go.mod file not found")); ok {
		t.Fatalf("non-go tool output must not classify as a Go module error")
	}
	// BUILD_FAILED without any ToolError: no report.
	if _, ok := For(phelixerr.New(phelixerr.CodeBuildFailed, "matrix build failed")); ok {
		t.Fatalf("structured BUILD_FAILED without tool evidence must stay unknown")
	}
}
