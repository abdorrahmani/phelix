package builder

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// execExitError returns a real *exec.ExitError so tests exercise the same
// cause type the builders preserve.
func execExitError(t *testing.T) *exec.ExitError {
	t.Helper()
	err := exec.Command("false").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError from test command, got %T", err)
	}
	return exitErr
}

func TestToolErrorUnwrapPreservesExitError(t *testing.T) {
	exitErr := execExitError(t)
	te := &ToolError{Tool: "go", Output: "some output", Err: exitErr}

	var got *exec.ExitError
	if !errors.As(error(te), &got) {
		t.Fatalf("errors.As lost the *exec.ExitError through ToolError")
	}
	if got != exitErr {
		t.Fatalf("unwrapped ExitError identity changed")
	}
	if te.Error() != exitErr.Error() {
		t.Fatalf("ToolError.Error() = %q, want underlying %q", te.Error(), exitErr.Error())
	}
}

func TestToolErrorUnderStructuredWrap(t *testing.T) {
	// The chain the go builder builds: *phelixerr.Error -> ToolError -> ExitError.
	exitErr := execExitError(t)
	err := phelixerr.Wrapf(phelixerr.CodeBuildFailed,
		&ToolError{Tool: "go", Output: "go.mod:1: unknown directive: modul", Err: exitErr},
		"go build failed for %s (%s)", "app", "go")

	if got := phelixerr.CodeOf(err); got != phelixerr.CodeBuildFailed {
		t.Fatalf("structured code lost: %s", got)
	}
	var exitErr2 *exec.ExitError
	if !errors.As(err, &exitErr2) {
		t.Fatalf("errors.As(err, *exec.ExitError) failed through the full chain")
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(err, *ToolError) failed through the wrap")
	}
	if te.Output == "" {
		t.Fatalf("captured output lost through the wrap")
	}
	// The captured output must never appear in the rendered message.
	if strings.Contains(err.Error(), "unknown directive") {
		t.Fatalf("tool output leaked into error message: %q", err.Error())
	}
}

func TestTailOutput(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"short passthrough", "line1\nline2\n"},
		{"empty", ""},
	}
	for _, tc := range cases {
		if got := tailOutput([]byte(tc.in)); got != strings.TrimLeft(tc.in, "\n") {
			t.Fatalf("%s: tailOutput = %q", tc.name, got)
		}
	}

	// Oversized input is truncated to the tail and starts on a line boundary.
	big := strings.Repeat("a", maxToolOutput) + "\n" + strings.Repeat("b", 100) + "\n"
	got := tailOutput([]byte(big))
	if len(got) > maxToolOutput {
		t.Fatalf("tail exceeds cap: %d", len(got))
	}
	if !strings.HasSuffix(got, strings.Repeat("b", 100)+"\n") {
		t.Fatalf("tail lost the most recent lines")
	}
	if strings.Contains(got, strings.Repeat("a", 50)) {
		t.Fatalf("tail kept the truncated head line: %q", got[:100])
	}
	if !strings.HasPrefix(got, strings.Repeat("b", 100)) {
		t.Fatalf("tail does not start on a line boundary: %q", got[:20])
	}
}
