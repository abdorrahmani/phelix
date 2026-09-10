package matrix

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// --- Task 5: matrix error-contract tests ----------------------------------

// TestRedactCommand verifies that build-arg VALUES are never emitted in the
// debug command log, while keys and structure remain recognizable.
func TestRedactCommand(t *testing.T) {
	args := []string{
		"build", "-t", "myapp:go1.22-amd64",
		"--build-arg", "GITHUB_TOKEN=ghp_xXxXsecretXxX",
		"--build-arg", "TARGETPLATFORM=linux/amd64",
		"--label", "org.label-schema=value",
		".",
	}
	got := redactCommand(args)
	if strings.Contains(got, "ghp_xXxXsecretXxX") {
		t.Fatalf("build-arg value leaked into redacted command: %s", got)
	}
	if !strings.Contains(got, "GITHUB_TOKEN=***") {
		t.Fatalf("expected key to remain with masked value, got: %s", got)
	}
	// The credential VALUE'S own prefix ("ghp_") must also be gone — the whole
	// value is masked, not just a substring of it.
	if strings.Contains(got, "ghp_") {
		t.Fatalf("credential prefix leaked through the mask: %s", got)
	}
	// Values that are not credentials also stay masked for uniformity.
	if !strings.Contains(got, "TARGETPLATFORM=***") {
		t.Fatalf("expected TARGETPLATFORM value masked: %s", got)
	}
	if !strings.Contains(got, "org.label-schema=***") {
		t.Fatalf("expected label value masked: %s", got)
	}
	if !strings.Contains(got, "-t myapp:go1.22-amd64") {
		t.Fatalf("non-build-arg args should survive unchanged: %s", got)
	}
}

// TestRedactCommand_PairedArgMode covers the separate-argument form
// "--build-arg", "KEY=value" (as opposed to the joined "KEY=value" form).
func TestRedactCommand_PairedArgMode(t *testing.T) {
	args := []string{"--build-arg", "DOCKER_PASS=supermagic", "--build-arg", "TARGETOS=linux"}
	got := redactCommand(args)
	if strings.Contains(got, "supermagic") {
		t.Fatalf("separate-arg build-arg value leaked: %s", got)
	}
	if !strings.Contains(got, "DOCKER_PASS=***") {
		t.Fatalf("expected DOCKER_PASS masked with key retained: %s", got)
	}
}

// TestPushImages_PartialFailureRedactsFailedList ensures the fail-closed error
// message carries the redacted per-combination message (no secrets leak out of
// the Failed() list).
func TestPushImages_PartialFailureCodeAndRedact(t *testing.T) {
	dmb := &DockerMatrixBuilder{ProjectRoot: t.TempDir()}
	results := []Result{
		{Combination: Combination{Version: "1.22", Platform: "linux/amd64"}, Status: "success", Artifact: "myapp:go1.22"},
		{Combination: Combination{Version: "1.23", Platform: "linux/amd64"}, Status: "failed",
			Error: errors.New("build failed: password=hunter2")},
	}
	err := dmb.PushImages(results, false)
	if err == nil {
		t.Fatal("expected push to be blocked on partial failure")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeDocker) {
		t.Fatalf("want DOCKER_ERROR, got %s", phelixerr.CodeOf(err))
	}
	// The per-combination error text is rendered through the fail-closed list,
	// which must not echo a credential back into the returned error.
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaked credential text: %s", err.Error())
	}
}

// TestPushImages_AllFailed_Code verifies the "nothing to push" arm.
func TestPushImages_AllFailed_Code(t *testing.T) {
	dmb := &DockerMatrixBuilder{ProjectRoot: t.TempDir()}
	results := []Result{
		{Combination: Combination{Version: "1.22"}, Status: "failed", Error: errors.New("e1")},
		{Combination: Combination{Version: "1.23"}, Status: "failed", Error: errors.New("e2")},
	}
	err := dmb.PushImages(results, true)
	if err == nil {
		t.Fatal("expected error when no images succeeded")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeDocker) {
		t.Fatalf("want DOCKER_ERROR, got %s", phelixerr.CodeOf(err))
	}
}

// TestBuildImageSimple_NoDocker preserves the exit-status cause: on a system
// without docker, the structured DOCKER_ERROR must wrap the exec "not found"
// error so errors.Is(err, exec.ErrNotFound) still works.
func TestBuildImageSimple_NoDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err == nil {
		t.Skip("docker present; this test requires an environment without docker")
	}
	_, err := BuildImageSimple(context.Background(), t.TempDir(), "test:ver", nil)
	if err == nil {
		t.Fatal("expected error building docker image without docker")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeDocker) {
		t.Fatalf("want DOCKER_ERROR, got %s", phelixerr.CodeOf(err))
	}
	if cause := phelixerr.Cause(err); !errors.Is(cause, exec.ErrNotFound) {
		t.Fatalf("expected exec.ErrNotFound root cause, got %v", cause)
	}
}

// TestBuildImageSimple_BuildArgsNotEchoed verifies the structured error does not
// include the raw docker output (which repeats build args) — instead the exit
// status cause is preserved.
func TestBuildImageSimple_BuildArgsNotEchoedInError(t *testing.T) {
	if _, err := exec.LookPath("docker"); err == nil {
		t.Skip("docker present; skip to avoid building a real image")
	}
	args := map[string]string{"TOKEN": "ghp_abcsecret"}
	r, err := BuildImageSimple(context.Background(), t.TempDir(), "test:ver", args)
	if err == nil {
		t.Fatalf("expected error, got success %+v", r)
	}
	if strings.Contains(err.Error(), "ghp_abcsecret") {
		t.Fatalf("build-arg value leaked into error: %s", err.Error())
	}
}

// TestParsePlan_InvalidArgs_CodeInvalidArgument pins the INVALID_ARGUMENT code
// for plan validation failures (no versions, no platforms, unknown version).
func TestParsePlan_InvalidArgs_CodeInvalidArgument(t *testing.T) {
	cases := []struct {
		name      string
		versions  []string
		platforms []string
	}{
		{"no versions", nil, []string{"linux/amd64"}},
		{"no platforms", []string{"1.22"}, nil},
		{"bad version", []string{"latest"}, []string{"linux/amd64"}},
		{"bad platform", []string{"1.22"}, []string{"riscv/foo"}},
	}
	for _, tc := range cases {
		_, err := ParsePlan(builder.Go, tc.versions, tc.platforms)
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		if !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
			t.Fatalf("%s: want INVALID_ARGUMENT, got %s (%v)", tc.name, phelixerr.CodeOf(err), err)
		}
	}
}
