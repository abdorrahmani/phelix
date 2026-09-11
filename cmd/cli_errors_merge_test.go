package cmd

import (
	"errors"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Interaction tests for the merged error renderer: the structured cause chain
// (renderCauseChain) and the Error Reporter (errreport) must compose without
// duplicating cause information or regressing exit codes.

func TestRenderKnownErrorNormalModeStillShowsCauseChain(t *testing.T) {
	stubInstallCommand(t, "sudo apt -y install golang")

	out, exit := renderForTest(t, toolchainChain(), false)
	if exit != ExitBuild {
		t.Fatalf("exit code changed: %d", exit)
	}
	// The report enriches the presentation...
	for _, want := range []string{"Suggested fix:", "sudo apt -y install golang", "Documentation:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
	// ...while the bounded cause chain stays the canonical cause rendering,
	// shown once even for known errors.
	if n := strings.Count(out, "not installed and installation was declined"); n != 1 {
		t.Fatalf("cause rendered %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "Cause:") == false && !strings.Contains(out, "Go toolchain (go) not installed") {
		t.Fatalf("cause chain missing from known-error output:\n%s", out)
	}
	// Known errors carry docs/fix, so they never point at --debug.
	if strings.Contains(out, "Run with --debug for the full error chain.") {
		t.Fatalf("known error must not point at --debug:\n%s", out)
	}
}

func TestRenderToolOutputAndCauseChainDoNotDuplicate(t *testing.T) {
	// goBuildChain: Error → ToolError → *exec.ExitError. The cause chain shows
	// the exit status; the raw compiler diagnostics belong to the Tool output
	// section only.
	out, exit := renderForTest(t, goBuildChain(t, "# example.com/app\ncmd/main.go:9:2: undefined: Foo"), false)
	if exit != ExitBuild {
		t.Fatalf("exit code changed: %d", exit)
	}
	if n := strings.Count(out, "undefined: Foo"); n != 1 {
		t.Fatalf("compiler diagnostic rendered %d times, want 1 (tool output section only):\n%s", n, out)
	}
	if !strings.Contains(out, "Tool output (most recent lines):") {
		t.Fatalf("tool output section missing:\n%s", out)
	}
	if !strings.Contains(out, "Cause:") || !strings.Contains(out, "exit status 1") {
		t.Fatalf("cause chain with exit status missing:\n%s", out)
	}
}

func TestRenderUsageErrorNotEnrichedByReporter(t *testing.T) {
	// Cobra arg/flag validation errors are plain errors: exit 2, usage code,
	// single headline, and no invented report or hint.
	out, exit := renderForTest(t, errors.New("accepts at most 1 arg(s), received 3"), false)
	if exit != ExitUsage {
		t.Fatalf("usage exit code changed: %d", exit)
	}
	for _, want := range []string{"INVALID_ARGUMENT"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage output missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"Suggested fix:", "Documentation:", "Run with --debug"} {
		if strings.Contains(out, banned) {
			t.Fatalf("usage error must not be enriched with %q:\n%s", banned, out)
		}
	}
	if n := strings.Count(out, "Error:"); n != 1 {
		t.Fatalf("expected one headline, got %d:\n%s", n, out)
	}
}

func TestRenderDebugShowsFullChainAndNoPointer(t *testing.T) {
	stubInstallCommand(t, "cmd")

	err := toolchainChain()
	for _, debug := range []bool{false, true} {
		out, exit := renderForTest(t, err, debug)
		if exit != ExitBuild {
			t.Fatalf("debug=%v: exit code changed: %d", debug, exit)
		}
		hasPointer := strings.Contains(out, "Run with --debug for the full error chain.")
		if debug && hasPointer {
			t.Fatalf("debug output must not point at --debug:\n%s", out)
		}
		// In debug mode the cause is always visible; the pointer is never
		// paired with the full chain (avoid duplicated guidance).
		if debug && !strings.Contains(out, "not installed") {
			t.Fatalf("debug output missing the cause chain:\n%s", out)
		}
	}

	// Unknown wrapped error in debug mode: full cause, no pointer.
	wrapped := phelixerr.Wrap(phelixerr.CodeConnection, "failed to connect", errors.New("connection refused"))
	out, _ := renderForTest(t, wrapped, true)
	if strings.Contains(out, "Run with --debug for the full error chain.") {
		t.Fatalf("debug output must not point at --debug:\n%s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Fatalf("debug output missing the cause:\n%s", out)
	}
}

func TestRenderRedactedCauseChainWithKnownError(t *testing.T) {
	// A secret embedded by a lower layer must never surface through the
	// report, the cause chain, or the tool output — in either mode.
	out1, _ := renderForTest(t, goBuildChain(t, "go: token=ghp_supersecretvalue in go.sum"), false)
	out2, _ := renderForTest(t, goBuildChain(t, "go: token=ghp_supersecretvalue in go.sum"), true)
	for _, out := range []string{out1, out2} {
		if strings.Contains(out, "ghp_supersecretvalue") {
			t.Fatalf("secret leaked through the merged renderer:\n%s", out)
		}
	}
}
