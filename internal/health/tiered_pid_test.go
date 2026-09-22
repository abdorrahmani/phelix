package health

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestDefaultPidAlive proves the liveness classification the docker non-root
// regression turned on: a signalable process is alive, a reaped/gone process is
// dead (ESRCH), and a process we exist-but-cannot-signal (EPERM) is ALIVE.
func TestDefaultPidAlive(t *testing.T) {
	if !defaultPidAlive(os.Getpid()) {
		t.Fatal("the current process must be reported alive")
	}

	// A reaped child no longer exists → signal 0 returns ESRCH → dead.
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.Process.Pid
	_ = cmd.Wait() // reap so the PID is fully gone, not a zombie
	if defaultPidAlive(dead) {
		t.Fatalf("reaped pid %d must be reported dead", dead)
	}

	// PID 1 always exists on Linux. Run as a non-root user (the container case),
	// signal 0 to it returns EPERM — owned by root, not signalable — and the old
	// code read that as dead, aborting every non-root container. It must be
	// ALIVE. As root the same call returns nil; either way the answer is alive.
	if runtime.GOOS == "linux" && !defaultPidAlive(1) {
		t.Fatal("pid 1 exists on linux; EPERM must not be read as dead")
	}
}

// okProbe is an HTTPProber that always answers with the given status.
func okProbe(status int) HTTPProber {
	return func(_ context.Context, _ string) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: http.NoBody}, nil
	}
}

// TestWaitForHealthy_AliveDoesNotFastFail: when alive() reports true (the fixed
// checker, or the docker container-state checker), WaitForHealthy must reach the
// probe and succeed — never returning ErrCandidateExited. This is the exact
// regression: a live candidate was being declared exited.
func TestWaitForHealthy_AliveDoesNotFastFail(t *testing.T) {
	res := &Resolver{
		pidAlive:  func(int) bool { return true },
		httpProbe: okProbe(200),
	}
	cfg := &DeployTierConfig{Mode: TierModeHTTP, Path: "/ping", Interval: "1ms", Retries: 1, Timeout: "1s"}
	if err := WaitForHealthy(context.Background(), Tier1HTTPPath, cfg, "127.0.0.1:8080", 4242, res); err != nil {
		t.Fatalf("a live candidate answering 200 must pass, got %v", err)
	}
}

// TestWaitForHealthy_DeadFailsFast: a dead pid must still fast-fail with
// ErrCandidateExited even when a probe would succeed — the fast-fail benefit is
// preserved.
func TestWaitForHealthy_DeadFailsFast(t *testing.T) {
	res := &Resolver{
		pidAlive:  func(int) bool { return false },
		httpProbe: okProbe(200),
	}
	cfg := &DeployTierConfig{Mode: TierModeHTTP, Path: "/ping", Interval: "1ms", Retries: 1, Timeout: "5s"}
	start := time.Now()
	err := WaitForHealthy(context.Background(), Tier1HTTPPath, cfg, "127.0.0.1:8080", 4242, res)
	if !errors.Is(err, ErrCandidateExited) {
		t.Fatalf("dead candidate must fail fast with ErrCandidateExited, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("dead candidate must fail fast, took %s", time.Since(start))
	}
}

// TestNewResolver_WithPidChecker covers the exported injection seam other
// packages (deploy) use to supply a docker container-state liveness checker.
func TestNewResolver_WithPidChecker(t *testing.T) {
	called := false
	r := NewResolver(WithPidChecker(func(int) bool { called = true; return true }))
	if !r.alive()(123) {
		t.Fatal("injected checker should report alive")
	}
	if !called {
		t.Fatal("NewResolver must wire WithPidChecker into alive()")
	}
	// A default resolver falls back to the real (EPERM-safe) checker.
	if NewResolver().alive() == nil {
		t.Fatal("default resolver must expose a non-nil alive checker")
	}
}
