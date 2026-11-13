package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/project"
)

func testRunner(t *testing.T, dir string) (*Runner, *project.HooksConfig) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := &project.HooksConfig{}
	return &Runner{
		AppName:         "api",
		AppID:           "app-1",
		ProjectDir:      dir,
		DeploymentID:    "dep-1",
		Strategy:        "blue-green",
		Version:         2,
		PreviousVersion: 1,
		Logger:          nopLogger{},
		Store:           NewStore("api"),
	}, cfg
}

func TestRunCapturesStreamsAndExitCode(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	cfg := &project.HookConfig{Command: []string{"sh", "-c", "echo out; echo err >&2; exit 3"}}

	result := runner.Run(context.Background(), project.HookPreDeploy, cfg)
	if result.Execution.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", result.Execution.Status)
	}
	if !strings.Contains(result.Execution.Stdout, "out") {
		t.Fatalf("stdout = %q", result.Execution.Stdout)
	}
	if !strings.Contains(result.Execution.Stderr, "err") {
		t.Fatalf("stderr = %q", result.Execution.Stderr)
	}
	if result.Execution.ExitCode == nil || *result.Execution.ExitCode != 3 {
		t.Fatalf("exit code = %v", result.Execution.ExitCode)
	}
	if !result.Aborted() {
		t.Fatal("a failed critical hook must abort")
	}
	if result.Execution.DurationMS < 0 {
		t.Fatal("duration must not be negative")
	}
}

func TestRunRetriesThenSucceeds(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	counter := filepath.Join(dir, "count")
	cfg := &project.HookConfig{
		Command: []string{"sh", "-c", "echo x >> " + counter + "; test $(wc -l < " + counter + ") -ge 2"},
		Retries: 1, RetryInterval: "1ms",
	}
	result := runner.Run(context.Background(), project.HookPostBuild, cfg)
	if result.Execution.Status != StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", result.Execution.Status)
	}
	if result.Execution.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", result.Execution.Attempt)
	}
}

func TestRunContinueOnErrorDoesNotAbort(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	keep := true
	cfg := &project.HookConfig{Command: []string{"sh", "-c", "exit 1"}, ContinueOnError: &keep}
	result := runner.Run(context.Background(), project.HookPostDeploy, cfg)
	if !result.Failed() {
		t.Fatal("execution should be recorded as failed")
	}
	if result.Aborted() {
		t.Fatal("continue_on_error must not abort the deployment")
	}
}

func TestRunTimeoutKillsProcess(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	cfg := &project.HookConfig{Command: []string{"sh", "-c", "sleep 10"}, Timeout: "50ms"}
	start := time.Now()
	result := runner.Run(context.Background(), project.HookPreTraffic, cfg)
	if result.Execution.Status != StatusTimedOut {
		t.Fatalf("status = %q, want timed_out", result.Execution.Status)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout did not terminate the process promptly")
	}
}

func TestRunCancellation(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	cfg := &project.HookConfig{Command: []string{"sh", "-c", "sleep 10"}, Timeout: "30s"}
	result := runner.Run(ctx, project.HookPreTraffic, cfg)
	if result.Execution.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", result.Execution.Status)
	}
}

func TestRunRedactsSecretValues(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	cfg := &project.HookConfig{
		Command: []string{"sh", "-c", "echo $API_TOKEN"},
		Env:     map[string]string{"API_TOKEN": "super-secret-value"},
	}
	result := runner.Run(context.Background(), project.HookPreDeploy, cfg)
	if strings.Contains(result.Execution.Stdout, "super-secret-value") {
		t.Fatalf("secret leaked: %q", result.Execution.Stdout)
	}
	if !strings.Contains(result.Execution.Stdout, "[REDACTED]") {
		t.Fatalf("expected redaction marker, got %q", result.Execution.Stdout)
	}
}

func TestRunDoesNotExecuteSucceededHookTwice(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	counter := filepath.Join(dir, "count")
	cfg := &project.HookConfig{Command: []string{"sh", "-c", "echo x >> " + counter}}

	first := runner.Run(context.Background(), project.HookPostStart, cfg)
	if first.Execution.Status != StatusSucceeded {
		t.Fatalf("first run status = %q", first.Execution.Status)
	}
	second := runner.Run(context.Background(), project.HookPostStart, cfg)
	if second.Execution.ID != first.Execution.ID {
		t.Fatalf("second run returned a new execution: %s != %s", second.Execution.ID, first.Execution.ID)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != 1 {
		t.Fatalf("hook executed %d times, want 1", lines)
	}
}

func TestRunRefusesUncertainPriorExecution(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	prior := &Execution{
		ID: "prior", AppName: "api", DeploymentID: "dep-1", Hook: project.HookPreDeploy,
		Status: StatusRunning, StartedAt: time.Now(),
	}
	if err := runner.Store.Save(prior); err != nil {
		t.Fatal(err)
	}
	cfg := &project.HookConfig{Command: []string{"sh", "-c", "echo should-not-run"}}
	result := runner.Run(context.Background(), project.HookPreDeploy, cfg)
	if result.Err == nil {
		t.Fatal("an uncertain prior execution must refuse to run again")
	}
	if result.Execution.Status != StatusUncertain {
		t.Fatalf("status = %q, want uncertain", result.Execution.Status)
	}
}

func TestRecoverMarksInterruptedExecutionsUncertain(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	prior := &Execution{ID: "prior", AppName: "api", DeploymentID: "dep-1", Hook: project.HookPostStart, Status: StatusRunning, StartedAt: time.Now()}
	if err := runner.Store.Save(prior); err != nil {
		t.Fatal(err)
	}
	recovered, err := runner.Store.Recover("dep-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Status != StatusUncertain {
		t.Fatalf("recovered = %+v", recovered)
	}
}

func TestWorkingDirEscapeIsRefused(t *testing.T) {
	dir := t.TempDir()
	runner, _ := testRunner(t, dir)
	cfg := &project.HookConfig{Command: []string{"true"}, WorkingDir: "../outside"}
	result := runner.Run(context.Background(), project.HookPreBuild, cfg)
	if result.Err == nil || result.Execution.Status != StatusFailed {
		t.Fatalf("expected refusal, got %+v", result)
	}
}
