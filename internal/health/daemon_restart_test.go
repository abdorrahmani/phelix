package health

import (
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
)

// restartStub records which restart path the auto-restarter chose.
type restartStub struct {
	app.AppManagerInterface
	classicCalled string
}

func (s *restartStub) RestartApplication(id string) error {
	s.classicCalled = id
	return nil
}

func withRestartHooks(t *testing.T, stub *restartStub, skipper func(string) bool, restarter func(string) error) {
	t.Helper()
	origMgr, origSkip, origRestart := app.Manager, app.DeployedAppSkipper, app.DeployedAppRestarter
	app.Manager, app.DeployedAppSkipper, app.DeployedAppRestarter = stub, skipper, restarter
	t.Cleanup(func() {
		app.Manager, app.DeployedAppSkipper, app.DeployedAppRestarter = origMgr, origSkip, origRestart
	})
}

// A classic app keeps the single-PID restart.
func TestRestartForAutoRestart_ClassicUsesAppManager(t *testing.T) {
	stub := &restartStub{}
	withRestartHooks(t, stub, func(string) bool { return false }, func(string) error {
		t.Fatal("deploy-aware restart used for a classic app")
		return nil
	})

	if err := restartForAutoRestart("app-1"); err != nil {
		t.Fatalf("restartForAutoRestart: %v", err)
	}
	if stub.classicCalled != "app-1" {
		t.Fatalf("RestartApplication called with %q, want %q", stub.classicCalled, "app-1")
	}
}

// A deploy-managed app must never reach RestartApplication: that kills the
// serving instance and then fights the proxy for the public port.
func TestRestartForAutoRestart_DeployedUsesDeployAwarePath(t *testing.T) {
	stub := &restartStub{}
	var deployed string
	withRestartHooks(t, stub, func(string) bool { return true }, func(id string) error {
		deployed = id
		return nil
	})

	if err := restartForAutoRestart("app-1"); err != nil {
		t.Fatalf("restartForAutoRestart: %v", err)
	}
	if deployed != "app-1" {
		t.Fatalf("deploy-aware restart called with %q, want %q", deployed, "app-1")
	}
	if stub.classicCalled != "" {
		t.Fatalf("RestartApplication was called with %q for a deploy-managed app", stub.classicCalled)
	}
}

// No wired deploy-aware restart is an error, not a silent classic restart.
func TestRestartForAutoRestart_DeployedWithoutHookFails(t *testing.T) {
	stub := &restartStub{}
	withRestartHooks(t, stub, func(string) bool { return true }, nil)

	if err := restartForAutoRestart("app-1"); err == nil {
		t.Fatal("expected an error when no deploy-aware restart is wired")
	}
	if stub.classicCalled != "" {
		t.Fatalf("fell back to RestartApplication (%q) for a deploy-managed app", stub.classicCalled)
	}
}

// No skipper wired at all (e.g. a non-monitor process) keeps the old behavior.
func TestRestartForAutoRestart_NoSkipperUsesAppManager(t *testing.T) {
	stub := &restartStub{}
	withRestartHooks(t, stub, nil, nil)

	if err := restartForAutoRestart("app-1"); err != nil {
		t.Fatalf("restartForAutoRestart: %v", err)
	}
	if stub.classicCalled != "app-1" {
		t.Fatalf("RestartApplication called with %q, want %q", stub.classicCalled, "app-1")
	}
}
