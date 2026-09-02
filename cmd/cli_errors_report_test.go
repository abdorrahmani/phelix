package cmd

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/errreport"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
)

// toolchainChain builds the error chain `phelix build` produces when the Go
// toolchain is missing (toolchain.EnsureTool error wrapped at the cmd site).
func toolchainChain() error {
	return phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain check failed",
		phelixerr.Newf(phelixerr.CodeToolchainNotFound,
			"%s not installed and installation was declined.",
			"Go toolchain (go)"))
}

// goBuildChain builds the chain internal/builder produces for a failed
// `go build`, with the given captured output.
func goBuildChain(t *testing.T, output string) error {
	t.Helper()
	err := exec.Command("false").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("test harness: expected *exec.ExitError")
	}
	return phelixerr.Wrapf(phelixerr.CodeBuildFailed,
		&builder.ToolError{Tool: "go", Output: output, Err: exitErr},
		"go build failed for %s (%s)", "app", "go")
}

func stubInstallCommand(t *testing.T, goCmd string) {
	t.Helper()
	old := errreport.InstallCommand
	errreport.InstallCommand = func(lang builder.Language) string {
		if lang == builder.Go {
			return goCmd
		}
		return ""
	}
	t.Cleanup(func() { errreport.InstallCommand = old })
}

func renderForTest(t *testing.T, err error, debug bool) (string, int) {
	t.Helper()
	old := errOut
	defer func() { errOut = old }()
	var buf bytes.Buffer
	errOut = &buf
	exit := RenderError(err, debug)
	return buf.String(), exit
}

func TestRenderKnownErrorToolchainReport(t *testing.T) {
	stubInstallCommand(t, "sudo apt -y install golang")

	out, exit := renderForTest(t, toolchainChain(), false)
	if exit != ExitBuild {
		t.Fatalf("exit code changed for known error: %d", exit)
	}
	for _, want := range []string{
		"toolchain check failed",
		"TOOLCHAIN_NOT_FOUND",
		"Go compiler (go) is not installed",
		"Suggested fix:",
		"sudo apt -y install golang",
		"Documentation:",
		"https://go.dev/doc/install",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "Error:") != 1 {
		t.Fatalf("error headline rendered more than once:\n%s", out)
	}
	// The generic hint is subsumed by the report — it must not also render.
	if strings.Contains(out, "Install the required toolchain, or allow Phelix") {
		t.Fatalf("generic hint duplicated alongside the report:\n%s", out)
	}
}

func TestRenderKnownErrorPortReport(t *testing.T) {
	oldFind := errreport.FindListener
	errreport.FindListener = func(portNum int) (phelixport.ProcessInfo, bool) {
		return phelixport.ProcessInfo{Name: "myapp", PID: 12345}, true
	}
	t.Cleanup(func() { errreport.FindListener = oldFind })

	err := phelixerr.Newf(phelixerr.CodePortUnavailable,
		"port %d is already in use by another process", 8080)
	out, exit := renderForTest(t, err, false)
	if exit != ExitNetwork {
		t.Fatalf("exit code changed for known port error: %d", exit)
	}
	for _, want := range []string{
		"PORT_UNAVAILABLE",
		"Port 8080 is already being used",
		"myapp",
		"12345",
		"Suggested fix:",
		"Run:",
		":8080",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "kill") {
		t.Fatalf("renderer must never suggest killing the process:\n%s", out)
	}
}

func TestRenderKnownErrorGoModReport(t *testing.T) {
	out, exit := renderForTest(t, goBuildChain(t,
		"go: example.com/app imports github.com/foo/bar: missing go.sum entry for module providing package github.com/foo/bar"), false)
	if exit != ExitBuild {
		t.Fatalf("exit code changed for known go.mod error: %d", exit)
	}
	for _, want := range []string{
		"BUILD_FAILED",
		"Missing go.sum entry",
		"Suggested fix:",
		"go mod tidy",
		"https://go.dev/ref/mod",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderUnknownBuildFailureShowsRawToolOutput(t *testing.T) {
	out, exit := renderForTest(t, goBuildChain(t,
		"# example.com/app\ncmd/main.go:9:2: undefined: Foo\ntoken=ghp_supersecretvalue"), false)
	if exit != ExitBuild {
		t.Fatalf("unknown build failure exit code changed: %d", exit)
	}
	if !strings.Contains(out, "Tool output") || !strings.Contains(out, "undefined: Foo") {
		t.Fatalf("raw tool output missing for unknown failure:\n%s", out)
	}
	if strings.Contains(out, "Suggested fix:") {
		t.Fatalf("unknown error must not invent a fix:\n%s", out)
	}
	if strings.Contains(out, "ghp_supersecretvalue") {
		t.Fatalf("secret leaked through the raw output section:\n%s", out)
	}
	// --debug pointer is shown when a deeper cause exists.
	if !strings.Contains(out, "--debug") {
		t.Fatalf("unknown error should point at --debug:\n%s", out)
	}
}

func TestRenderUnknownNonToolErrorUnchanged(t *testing.T) {
	// Plain wrapped error without tool output: hint renders, no tool section.
	err := phelixerr.Wrap(phelixerr.CodeConnection, "failed to connect", errors.New("connection refused"))
	out, exit := renderForTest(t, err, false)
	if exit != ExitNetwork {
		t.Fatalf("exit code changed: %d", exit)
	}
	if strings.Contains(out, "Tool output") || strings.Contains(out, "Suggested fix:") {
		t.Fatalf("unexpected enrichment on a plain error:\n%s", out)
	}
	if !strings.Contains(out, "--debug") {
		t.Fatalf("wrapped error should point at --debug for the cause:\n%s", out)
	}
}

func TestRenderDebugStillShowsRootCauseForKnownErrors(t *testing.T) {
	stubInstallCommand(t, "cmd")

	err := phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain check failed",
		phelixerr.Newf(phelixerr.CodeToolchainNotFound, "%s not installed", "Go toolchain (go)"))
	out, exit := renderForTest(t, err, true)
	if exit != ExitBuild {
		t.Fatalf("exit code changed in debug mode: %d", exit)
	}
	// The friendly report is an additional layer, not a replacement.
	for _, want := range []string{"Suggested fix:", "Cause:", "not installed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderDebugNoSecretLeakInReport(t *testing.T) {
	// A hostile-looking process name or tool output must be redacted before
	// rendering, in normal and debug mode alike.
	oldFind := errreport.FindListener
	errreport.FindListener = func(portNum int) (phelixport.ProcessInfo, bool) {
		return phelixport.ProcessInfo{Name: "myapp token=ghp_abcdef123456", PID: 7}, true
	}
	t.Cleanup(func() { errreport.FindListener = oldFind })

	err := phelixerr.Newf(phelixerr.CodePortUnavailable,
		"port %d is already in use by another process", 8080)
	for _, debug := range []bool{false, true} {
		out, _ := renderForTest(t, err, debug)
		if strings.Contains(out, "ghp_abcdef123456") {
			t.Fatalf("secret leaked (debug=%v):\n%s", debug, out)
		}
	}
}

func TestExitCodesUnchangedForKnownErrors(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{toolchainChain(), ExitBuild},            // 20
		{goBuildChain(t, "whatever"), ExitBuild}, // 20
		{phelixerr.Newf(phelixerr.CodePortUnavailable, "port %d is already in use by another process", 8080), ExitNetwork}, // 30
	}
	for i, tc := range cases {
		if got := ExitCodeFor(tc.err); got != tc.want {
			t.Fatalf("case %d: ExitCodeFor = %d, want %d", i, got, tc.want)
		}
	}
}

func TestRenderOnceWithReport(t *testing.T) {
	stubInstallCommand(t, "cmd")
	err := toolchainChain()
	for _, debug := range []bool{false, true} {
		out, _ := renderForTest(t, err, debug)
		if n := strings.Count(out, "Error:"); n != 1 {
			t.Fatalf("debug=%v: expected one headline, got %d:\n%s", debug, n, out)
		}
		if n := strings.Count(out, "TOOLCHAIN_NOT_FOUND"); n != 1 {
			t.Fatalf("debug=%v: expected one code line, got %d:\n%s", debug, n, out)
		}
	}
}
