package errreport

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// buildFail constructs the error chain a Go/Rust builder produces on tool
// failure: phelixerr.Error(BUILD_FAILED) -> builder.ToolError -> *exec.ExitError.
func buildFail(tool, output string) error {
	err := exec.Command("false").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		panic("test harness: expected *exec.ExitError")
	}
	return phelixerr.Wrapf(phelixerr.CodeBuildFailed,
		&builder.ToolError{Tool: tool, Output: output, Err: exitErr},
		"%s build failed for app (%s)", tool, tool)
}

func TestForUnknownErrorsStayUnknown(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errors.New("something went sideways")},
		{"build failed without tool evidence", phelixerr.New(phelixerr.CodeBuildFailed, "matrix build failed")},
		{"compile error is not a module error", buildFail("go", "# example.com/app\ncmd/main.go:9:2: undefined: Foo")},
		{"linker failure", buildFail("go", "# example.com/app\n/usr/bin/ld: cannot find -lfoo")},
		{"cargo failure", buildFail("cargo", "error[E0432]: unresolved import `foo`")},
		{"toolchain error without tool identity",
			phelixerr.Wrapf(phelixerr.CodeToolchainNotFound,
				errors.New("the `cross` tool is required for Rust cross-compilation but was not found"),
				"toolchain check failed")},
		{"validation error", phelixerr.New(phelixerr.CodeValidation, "invalid port 99999")},
		{"broad keywords must not classify", buildFail("go", "error: failed to build application")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rep, ok := For(tc.err); ok {
				t.Fatalf("unexpected report for %s: %+v", tc.name, rep)
			}
		})
	}
}

func TestToolOutput(t *testing.T) {
	err := buildFail("go", "# phelix\ncmd/main.go:9:2: undefined: Foo")
	out, ok := ToolOutput(err)
	if !ok || !containsAny(out, "undefined: Foo") {
		t.Fatalf("ToolOutput lost captured output: %q ok=%v", out, ok)
	}

	if _, ok := ToolOutput(phelixerr.New(phelixerr.CodeBuildFailed, "no tool error here")); ok {
		t.Fatalf("ToolOutput reported output for an error without ToolError")
	}
	te := &builder.ToolError{Tool: "go", Err: errors.New("exit status 1")}
	if _, ok := ToolOutput(phelixerr.Wrapf(phelixerr.CodeBuildFailed, te, "wrapped")); ok {
		t.Fatalf("ToolOutput should skip an empty captured output")
	}
}

func TestChainTextIncludesInnerMessages(t *testing.T) {
	err := phelixerr.Wrap(phelixerr.CodeBuildFailed, "outer",
		phelixerr.Wrap(phelixerr.CodeBuildFailed, "middle", errors.New("root")))
	text := chainText(err)
	for _, want := range []string{"outer", "middle", "root"} {
		if !containsAny(text, want) {
			t.Fatalf("chainText missing %q: %q", want, text)
		}
	}
}

func TestRegisteredResolverPrecedence(t *testing.T) {
	// Register prepends: the new resolver wins over the built-ins.
	old := resolvers
	t.Cleanup(func() { resolvers = old })

	resolvers = append([]Resolver(nil), old...) // isolate from other tests
	Register(resolverFunc(func(err error) (Report, bool) {
		if phelixerr.IsCode(err, phelixerr.CodeToolchainNotFound) {
			return Report{Code: phelixerr.CodeToolchainNotFound, Title: "custom"}, true
		}
		return Report{}, false
	}))

	err := phelixerr.New(phelixerr.CodeToolchainNotFound, "Go toolchain (go) not installed")
	rep, ok := For(err)
	if !ok || rep.Title != "custom" {
		t.Fatalf("registered resolver did not take precedence: %+v ok=%v", rep, ok)
	}
}

type resolverFunc func(err error) (Report, bool)

func (f resolverFunc) Resolve(err error) (Report, bool) { return f(err) }

func TestReportCodeEchoesExistingCode(t *testing.T) {
	// Resolvers never remap codes; For fills in whatever the error carries.
	err := buildFail("go", "go: missing go.sum entry for module providing package foo")
	rep, ok := For(err)
	if !ok {
		t.Fatalf("expected known report")
	}
	if rep.Code != phelixerr.CodeBuildFailed {
		t.Fatalf("report code = %s, want BUILD_FAILED", rep.Code)
	}
}
