package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"unknown", errors.New("boom"), ExitFailure},
		{"notfound", phelixerr.New(phelixerr.CodeNotFound, "nope"), ExitNotFound},
		{"auth", phelixerr.New(phelixerr.CodeUnauthenticated, "auth"), ExitAuth},
		{"permission", phelixerr.New(phelixerr.CodePermissionDenied, "perm"), ExitPermission},
		{"build", phelixerr.New(phelixerr.CodeBuildFailed, "build"), ExitBuild},
		{"deploy", phelixerr.New(phelixerr.CodeDeployFailed, "deploy"), ExitDeploy},
		{"rollback", phelixerr.New(phelixerr.CodeRollbackFailed, "rb"), ExitRollback},
		{"network", phelixerr.New(phelixerr.CodeConnection, "net"), ExitNetwork},
		{"config", phelixerr.New(phelixerr.CodeConfiguration, "cfg"), ExitConfig},
		{"docker", phelixerr.New(phelixerr.CodeDocker, "dck"), ExitDocker},
		{"timeout", phelixerr.New(phelixerr.CodeTimeout, "to"), ExitTimeout},
		{"encryption", phelixerr.New(phelixerr.CodeEncryption, "enc"), ExitEncryption},
		{"invalidarg", phelixerr.New(phelixerr.CodeInvalidArgument, "arg"), ExitUsage},
		{"validation", phelixerr.New(phelixerr.CodeValidation, "v"), ExitUsage},
		{"wrapped", phelixerr.Wrap(phelixerr.CodeBuildFailed, "wrap", errors.New("root")), ExitBuild},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.err); got != tc.want {
				t.Fatalf("ExitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestExitCodeForCobraUsageErrors(t *testing.T) {
	// Cobra's arg/flag validation errors are plain errors; they must map to
	// the usage exit code so scripts can distinguish "bad invocation" from
	// "operation failed".
	cases := []struct {
		name string
		err  error
	}{
		{"minimum args", errors.New("requires at least 2 arg(s), only received 1")},
		{"exact args", errors.New("accepts 1 arg(s), received 2")},
		{"maximum args", errors.New("accepts at most 1 arg(s), received 3")},
		{"between args", errors.New("accepts between 2 and 4 arg(s), received 5")},
		{"required flag", errors.New(`required flag(s) "name" not set`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.err); got != ExitUsage {
				t.Fatalf("ExitCodeFor(%q) = %d, want %d", tc.err.Error(), got, ExitUsage)
			}
			// And the rendered code reads INVALID_ARGUMENT, not UNKNOWN.
			var buf bytes.Buffer
			old := errOut
			errOut = &buf
			RenderError(tc.err, false)
			errOut = old
			if !strings.Contains(buf.String(), "INVALID_ARGUMENT") {
				t.Fatalf("usage error should render INVALID_ARGUMENT, got: %q", buf.String())
			}
		})
	}

	// Non-usage plain errors must NOT be classified as usage.
	if got := ExitCodeFor(errors.New("requires a lot of effort")); got != ExitFailure {
		t.Fatalf("unrelated plain error classified as usage: %d", got)
	}
}

func TestExitCodeForPreferredOuterCode(t *testing.T) {
	// Inner build failure wrapped by deploy: the outer, most specific code wins.
	err := phelixerr.Wrap(phelixerr.CodeDeployFailed, "deploy", phelixerr.New(phelixerr.CodeBuildFailed, "build"))
	if got := ExitCodeFor(err); got != ExitDeploy {
		t.Fatalf("expected EXIT_DEPLOY from outer code, got %d", got)
	}
}

func TestRenderErrorReturnsExitCode(t *testing.T) {
	old := errOut
	defer func() { errOut = old }()
	errOut = &bytes.Buffer{}

	for _, tc := range []struct {
		err  error
		want int
	}{
		{phelixerr.New(phelixerr.CodeNotFound, "app not found"), ExitNotFound},
		{phelixerr.New(phelixerr.CodeConnection, "conn"), ExitNetwork},
		{errors.New("plain"), ExitFailure},
	} {
		if got := RenderError(tc.err, false); got != tc.want {
			t.Fatalf("RenderError(%v) returned %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestRenderErrorFormat(t *testing.T) {
	old := errOut
	defer func() { errOut = old }()
	var buf bytes.Buffer
	errOut = &buf

	exit := RenderError(phelixerr.New(phelixerr.CodeNotFound, "application not found"), false)
	if exit != ExitNotFound {
		t.Fatalf("unexpected exit code %d", exit)
	}

	out := buf.String()
	if !strings.Contains(out, "application not found") {
		t.Fatalf("message missing from output: %q", out)
	}
	if !strings.Contains(out, "NOT_FOUND") {
		t.Fatalf("code missing from output: %q", out)
	}
	// Actionable hint should mention phelix list for NOT_FOUND.
	if !strings.Contains(out, "phelix list") {
		t.Fatalf("actionable hint missing from output: %q", out)
	}
}

func TestRenderErrorDebugShowsCause(t *testing.T) {
	old := errOut
	defer func() { errOut = old }()
	var buf bytes.Buffer
	errOut = &buf

	root := errors.New("connection refused")
	err := phelixerr.Wrap(phelixerr.CodeConnection, "failed to connect to server", root)

	RenderError(err, true)
	out := buf.String()
	if !strings.Contains(out, "connection refused") {
		t.Fatalf("root cause missing in debug output: %q", out)
	}
	if !strings.Contains(out, "CONNECTION_ERROR") {
		t.Fatalf("code missing in debug output: %q", out)
	}
}

func TestRenderErrorNoCauseLeak(t *testing.T) {
	old := errOut
	defer func() { errOut = old }()

	secrets := []string{
		"sk-abcdef1234567890abcdef1234567890",
		"ghp_abcdef1234567890",
		"token=abcdef1234567890",
		"apiKey=abcdef1234567890",
		"password=hunter2",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9",
	}

	for _, secret := range secrets {
		var buf bytes.Buffer
		errOut = &buf
		root := errors.New("unstructured " + secret)
		err := phelixerr.Wrap(phelixerr.CodeUnauthenticated, "auth failed", root)

		RenderError(err, false)
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("secret %q leaked into normal output: %q", secret, buf.String())
		}

		buf.Reset()
		RenderError(err, true)
		if strings.Contains(buf.String(), secret) {
			t.Fatalf("secret %q leaked into debug output: %q", secret, buf.String())
		}
	}
}

func TestHintFor(t *testing.T) {
	cases := []struct {
		code phelixerr.Code
		want string
	}{
		{phelixerr.CodeUnauthenticated, "phelix auth login"},
		{phelixerr.CodeNotFound, "phelix list"},
		{phelixerr.CodeHealthCheckFailed, "phelix health status"},
		{phelixerr.CodeDeployLocked, "phelix deploy unlock"},
		{phelixerr.CodeToolchainNotFound, "toolchain"},
		{phelixerr.CodeServer, ""},
		{phelixerr.CodeUnknown, ""},
	}
	for _, tc := range cases {
		h := hintFor(phelixerr.New(tc.code, "msg"))
		if tc.want == "" {
			if h != "" {
				t.Fatalf("code %s: expected no hint, got %q", tc.code, h)
			}
			continue
		}
		if !strings.Contains(h, tc.want) {
			t.Fatalf("code %s: hint %q missing %q", tc.code, h, tc.want)
		}
	}
}

func TestRenderErrorWritesToStderrNotStdout(t *testing.T) {
	// The renderer must write to errOut (production: stderr) and never to
	// stdout, so success output and pipeline data are not polluted.
	old := errOut
	defer func() { errOut = old }()

	// Redirect stdout to a temp file so we can assert nothing lands there.
	tmp, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("create temp stdout: %v", err)
	}
	defer tmp.Close()
	oldStdout := os.Stdout
	os.Stdout = tmp
	defer func() { os.Stdout = oldStdout }()

	var stderr bytes.Buffer
	errOut = &stderr

	err = phelixerr.New(phelixerr.CodeBuildFailed, "build exploded")
	if got := RenderError(err, false); got != ExitBuild {
		t.Fatalf("unexpected exit code %d", got)
	}

	if !strings.Contains(stderr.String(), "build exploded") {
		t.Fatalf("error message missing from stderr: %q", stderr.String())
	}

	if fi, _ := tmp.Stat(); fi.Size() != 0 {
		t.Fatalf("stdout polluted: %q", readAll(t, tmp))
	}
}

// readAll reads the remaining contents of f from the start.
func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestRenderErrorSingleErrorMessage(t *testing.T) {
	// Exactly one final error message: the headline may span a wrapped cause in
	// debug mode, but never multiple independent "Error:" blocks.
	old := errOut
	defer func() { errOut = old }()
	var buf bytes.Buffer
	errOut = &buf

	root := errors.New("root cause")
	err := phelixerr.Wrap(
		phelixerr.CodeDeployFailed,
		"deploy failed",
		phelixerr.Wrap(phelixerr.CodeBuildFailed, "build failed", root),
	)

	for _, debug := range []bool{false, true} {
		buf.Reset()
		RenderError(err, debug)
		n := strings.Count(buf.String(), "Error:")
		if n != 1 {
			t.Fatalf("debug=%v: expected exactly one error headline, got %d: %q", debug, n, buf.String())
		}
	}
}

func TestRenderErrorUnknownErrorGraceful(t *testing.T) {
	// A plain, non-structured error must render a generic message and exit
	// code WITHOUT panicking, and its text preserved for --debug inspection.
	old := errOut
	defer func() { errOut = old }()
	var buf bytes.Buffer
	errOut = &buf

	plain := errors.New("something went sideways")
	if got := RenderError(plain, true); got != ExitFailure {
		t.Fatalf("plain error exit = %d, want %d", got, ExitFailure)
	}
	if !strings.Contains(buf.String(), "something went sideways") {
		t.Fatalf("plain error text missing from debug output: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "UNKNOWN") {
		t.Fatalf("plain error should render UNKNOWN code: %q", buf.String())
	}
}

func TestSplitLines(t *testing.T) {
	lines := splitLines("Run:\n  phelix auth login")
	if len(lines) != 2 || lines[0] != "Run:" || lines[1] != "  phelix auth login" {
		t.Fatalf("unexpected split: %#v", lines)
	}
}

func TestRenderErrorShowsRootCauseInNormalMode(t *testing.T) {
	// Regression for "prepare deploy artifact failed" dead ends: the root
	// cause must be visible WITHOUT --debug, and the headline must still be
	// printed exactly once.
	old := errOut
	defer func() { errOut = old }()
	var buf bytes.Buffer
	errOut = &buf

	err := phelixerr.Wrap(
		phelixerr.CodeBuildFailed,
		"prepare deploy artifact failed",
		phelixerr.Wrap(phelixerr.CodeBuildFailed, "rebuild failed",
			phelixerr.New(phelixerr.CodeBuildFailed, "cargo build exited with status 101: error[E0425]")),
	)

	RenderError(err, false)
	out := buf.String()
	if !strings.Contains(out, "cargo build exited with status 101: error[E0425]") {
		t.Fatalf("root cause missing from normal-mode output: %q", out)
	}
	if !strings.Contains(out, "rebuild failed") {
		t.Fatalf("intermediate cause missing from normal-mode output: %q", out)
	}
	if n := strings.Count(out, "Error:"); n != 1 {
		t.Fatalf("expected exactly one Error headline, got %d: %q", n, out)
	}
}

func TestRenderErrorNoCauseNoCauseBlock(t *testing.T) {
	old := errOut
	defer func() { errOut = old }()
	var buf bytes.Buffer
	errOut = &buf

	RenderError(phelixerr.New(phelixerr.CodeNotFound, "app not found"), false)
	if strings.Contains(buf.String(), "Cause:") {
		t.Fatalf("cause-less error must not render a Cause block: %q", buf.String())
	}
}

func TestCauseMessagesDedupAndOrder(t *testing.T) {
	err := phelixerr.Wrap(phelixerr.CodeBuildFailed, "outer",
		phelixerr.Wrap(phelixerr.CodeBuildFailed, "mid", // re-wraps same message
			errors.New("root")))
	err = phelixerr.Wrap(phelixerr.CodeBuildFailed, "outer", err) // duplicate headline

	got := causeMessages(err)
	want := []string{"mid", "root"}
	if len(got) != len(want) {
		t.Fatalf("causeMessages = %#v, want %v (dedup + below-headline only)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("causeMessages = %#v, want %v", got, want)
		}
	}
}

func TestRenderErrorCauseChainBounded(t *testing.T) {
	old := errOut
	defer func() { errOut = old }()
	var buf bytes.Buffer
	errOut = &buf

	var err error = phelixerr.New(phelixerr.CodeBuildFailed, "layer 0")
	for i := 1; i <= 10; i++ {
		err = phelixerr.Wrap(phelixerr.CodeBuildFailed, fmt.Sprintf("layer %d", i), err)
	}

	RenderError(err, false)
	normal := buf.String()

	buf.Reset()
	RenderError(err, true)
	debug := buf.String()

	// Cause lines are two-space indented; the headline also contains
	// "layer 10" but is not part of the Cause block.
	if n := strings.Count(normal, "\n  layer"); n > causeMaxMessagesNormal {
		t.Fatalf("normal output unbounded: %d cause lines shown", n)
	}
	if !strings.Contains(debug, "layer 0") {
		t.Fatalf("debug output must reach the deepest layer: %q", debug)
	}
}
