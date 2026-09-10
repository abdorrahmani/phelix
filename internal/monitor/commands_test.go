package monitor

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

// stubManager records lifecycle calls so tests can assert which app-manager
// method a remote command dispatched to.
type stubManager struct {
	app.AppManagerInterface

	startCalled   string
	removeCalled  string
	stopCalled    string
	restartCalled string
	removeErr     error
}

func (s *stubManager) ListApplications() []app.AppListItem {
	return []app.AppListItem{{ID: "app-123", Name: "web", Port: 8080}}
}

func (s *stubManager) StartApplication(id string, _ int, _ string) error {
	s.startCalled = id
	return nil
}
func (s *stubManager) StopApplication(id string) error    { s.stopCalled = id; return nil }
func (s *stubManager) RestartApplication(id string) error { s.restartCalled = id; return nil }
func (s *stubManager) RemoveApplication(id string) error {
	s.removeCalled = id
	return s.removeErr
}

func (s *stubManager) touched() string {
	return strings.Join([]string{s.startCalled, s.stopCalled, s.restartCalled, s.removeCalled}, "")
}

func withStubManager(t *testing.T, stub *stubManager) {
	t.Helper()
	// Deploy state is read from $HOME/.phelix; isolate it so routing decisions
	// never depend on what the developer's machine has deployed.
	t.Setenv("HOME", t.TempDir())
	orig := app.Manager
	app.Manager = stub
	t.Cleanup(func() { app.Manager = orig })
}

func TestExecuteRemoveCommand(t *testing.T) {
	stub := &stubManager{}
	withStubManager(t, stub)

	exec := NewCommandExecutor()
	err := exec.Execute(Command{
		Type:    "remove",
		Payload: CommandPayload{Type: "remove", AppName: "web"},
	})
	if err != nil {
		t.Fatalf("Execute(remove) returned error: %v", err)
	}
	if stub.removeCalled != "app-123" {
		t.Fatalf("RemoveApplication called with %q, want %q", stub.removeCalled, "app-123")
	}
	if stub.stopCalled != "" || stub.restartCalled != "" {
		t.Fatalf("unexpected extra calls: stop=%q restart=%q", stub.stopCalled, stub.restartCalled)
	}
}

func TestExecuteRemoveCommand_ResolvesByID(t *testing.T) {
	stub := &stubManager{}
	withStubManager(t, stub)

	exec := NewCommandExecutor()
	err := exec.Execute(Command{
		Type:    "remove",
		Payload: CommandPayload{Type: "remove", AppName: "app-123"},
	})
	if err != nil {
		t.Fatalf("Execute(remove by ID) returned error: %v", err)
	}
	if stub.removeCalled != "app-123" {
		t.Fatalf("RemoveApplication called with %q, want %q", stub.removeCalled, "app-123")
	}
}

func TestExecuteRemoveCommand_PropagatesError(t *testing.T) {
	stub := &stubManager{removeErr: errors.New("stop failed")}
	withStubManager(t, stub)

	exec := NewCommandExecutor()
	err := exec.Execute(Command{
		Type:    "remove",
		Payload: CommandPayload{Type: "remove", AppName: "web"},
	})
	if err == nil {
		t.Fatal("Execute(remove) should propagate RemoveApplication error")
	}
}

func TestExecuteUnknownApp_Fails(t *testing.T) {
	withStubManager(t, &stubManager{})

	exec := NewCommandExecutor()
	err := exec.Execute(Command{
		Type:    "remove",
		Payload: CommandPayload{Type: "remove", AppName: "ghost"},
	})
	if err == nil {
		t.Fatal("Execute for unknown app should fail")
	}
}

// TestRebuildOverrideArgs pins the backend override → `phelix rebuild` flag
// mapping: no override adds no flags (phelix.yaml still decides), a valid one
// becomes --strategy [--replicas N], and anything the CLI cannot honor exactly
// is an error rather than a silent downgrade.
func TestRebuildOverrideArgs(t *testing.T) {
	cases := []struct {
		name    string
		payload CommandPayload
		want    []string
		wantErr bool
	}{
		{name: "no override", payload: CommandPayload{Type: "rebuild", AppName: "web"}},
		{name: "classic", payload: CommandPayload{Type: "rebuild", Strategy: "classic"}, want: []string{"--strategy", "classic"}},
		{name: "blue-green", payload: CommandPayload{Type: "rebuild", Strategy: "blue-green"}, want: []string{"--strategy", "blue-green"}},
		{name: "rolling without replicas", payload: CommandPayload{Type: "rebuild", Strategy: "rolling"}, want: []string{"--strategy", "rolling"}},
		{name: "rolling with replicas", payload: CommandPayload{Type: "rebuild", Strategy: "rolling", Replicas: 3}, want: []string{"--strategy", "rolling", "--replicas", "3"}},
		{name: "canary", payload: CommandPayload{Type: "rebuild", Strategy: "canary"}, want: []string{"--strategy", "canary"}},
		{name: "progressive", payload: CommandPayload{Type: "rebuild", Strategy: "progressive"}, want: []string{"--strategy", "progressive"}},

		{name: "unknown strategy", payload: CommandPayload{Type: "rebuild", Strategy: "surge"}, wantErr: true},
		{name: "replicas on blue-green", payload: CommandPayload{Type: "rebuild", Strategy: "blue-green", Replicas: 2}, wantErr: true},
		{name: "replicas on canary", payload: CommandPayload{Type: "rebuild", Strategy: "canary", Replicas: 2}, wantErr: true},
		{name: "replicas without a strategy", payload: CommandPayload{Type: "rebuild", Replicas: 2}, wantErr: true},
		{name: "negative replicas", payload: CommandPayload{Type: "rebuild", Strategy: "rolling", Replicas: -1}, wantErr: true},
		{name: "override on restart", payload: CommandPayload{Type: "restart", Strategy: "rolling", Replicas: 2}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rebuildOverrideArgs(tc.payload)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got args %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("args = %v, want %v", got, tc.want)
			}
		})
	}
}

// An override the executor rejects must never reach the app manager: a
// restart carrying a strategy is a backend/agent disagreement, not a restart.
func TestExecuteRejectsOverrideOnLifecycleCommand(t *testing.T) {
	stub := &stubManager{}
	withStubManager(t, stub)

	err := NewCommandExecutor().Execute(Command{
		Type:    "restart",
		Payload: CommandPayload{Type: "restart", AppName: "web", Strategy: "rolling", Replicas: 2},
	})
	if err == nil {
		t.Fatal("expected Execute to reject a lifecycle command carrying deployment overrides")
	}
	if stub.restartCalled != "" {
		t.Fatalf("RestartApplication was called with %q despite the rejection", stub.restartCalled)
	}
}

// captureFallback replaces the subprocess runner with a recorder, so routing is
// asserted without spawning anything (the daemon's own executable under test is
// the test binary).
func captureFallback(t *testing.T) *[]string {
	t.Helper()
	var args []string
	orig := runPhelixCommand
	runPhelixCommand = func(c *exec.Cmd) error {
		args = append([]string(nil), c.Args[1:]...)
		return nil
	}
	t.Cleanup(func() { runPhelixCommand = orig })
	return &args
}

// A zero-downtime deployment owns its instances, their internal ports and the
// proxy route. Handing start/stop/restart/remove to the single-PID app manager
// killed the serving instance and then tried to bind the proxy-owned public
// port, so the app went down while the backend was told the command succeeded.
// Those commands must route to the CLI, which is deploy-aware.
func TestExecuteDeployManagedAppRoutesToCLI(t *testing.T) {
	for _, mode := range []deploy.Mode{deploy.ModeBlueGreen, deploy.ModeRolling} {
		for _, typ := range []string{"start", "stop", "restart", "remove"} {
			t.Run(string(mode)+"/"+typ, func(t *testing.T) {
				stub := &stubManager{}
				withStubManager(t, stub) // isolates $HOME; seed state after it
				if err := deploy.Store(&deploy.DeployState{AppName: "web", Mode: mode, PublicPort: 8080}); err != nil {
					t.Fatalf("seed deploy state: %v", err)
				}
				args := captureFallback(t)

				if err := NewCommandExecutor().Execute(Command{
					Type:    typ,
					Payload: CommandPayload{Type: typ, AppName: "web"},
				}); err != nil {
					t.Fatalf("Execute(%s) returned error: %v", typ, err)
				}
				if got := stub.touched(); got != "" {
					t.Fatalf("app manager was called (%q) for a deploy-managed app", got)
				}
				if want := strings.Join([]string{typ, "app-123"}, " "); strings.Join(*args, " ") != want {
					t.Fatalf("ran `phelix %s`, want `phelix %s`", strings.Join(*args, " "), want)
				}
			})
		}
	}
}

// Classic apps keep the in-process dispatch: no deploy.json means no proxy
// route and no instances to reconcile, and the app manager works regardless of
// the daemon's environment.
func TestExecuteClassicAppStaysInProcess(t *testing.T) {
	stub := &stubManager{}
	withStubManager(t, stub)
	args := captureFallback(t)

	if err := NewCommandExecutor().Execute(Command{
		Type:    "restart",
		Payload: CommandPayload{Type: "restart", AppName: "web"},
	}); err != nil {
		t.Fatalf("Execute(restart) returned error: %v", err)
	}
	if stub.restartCalled != "app-123" {
		t.Fatalf("RestartApplication called with %q, want %q", stub.restartCalled, "app-123")
	}
	if len(*args) != 0 {
		t.Fatalf("classic restart shelled out to `phelix %s`", strings.Join(*args, " "))
	}
}

// A classic-mode deploy.json is not a zero-downtime deployment; only
// blue-green/rolling own a proxy route.
func TestExecuteClassicDeployStateStaysInProcess(t *testing.T) {
	stub := &stubManager{}
	withStubManager(t, stub) // isolates $HOME; seed state after it
	if err := deploy.Store(&deploy.DeployState{AppName: "web", Mode: "classic", PublicPort: 8080}); err != nil {
		t.Fatalf("seed deploy state: %v", err)
	}
	args := captureFallback(t)

	if err := NewCommandExecutor().Execute(Command{
		Type:    "stop",
		Payload: CommandPayload{Type: "stop", AppName: "web"},
	}); err != nil {
		t.Fatalf("Execute(stop) returned error: %v", err)
	}
	if stub.stopCalled != "app-123" {
		t.Fatalf("StopApplication called with %q, want %q", stub.stopCalled, "app-123")
	}
	if len(*args) != 0 {
		t.Fatalf("classic stop shelled out to `phelix %s`", strings.Join(*args, " "))
	}
}
