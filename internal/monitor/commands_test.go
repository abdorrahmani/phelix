package monitor

import (
	"errors"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
)

// stubManager records lifecycle calls so tests can assert which app-manager
// method a remote command dispatched to.
type stubManager struct {
	app.AppManagerInterface

	removeCalled  string
	stopCalled    string
	restartCalled string
	removeErr     error
}

func (s *stubManager) ListApplications() []app.AppListItem {
	return []app.AppListItem{{ID: "app-123", Name: "web", Port: 8080}}
}

func (s *stubManager) StopApplication(id string) error    { s.stopCalled = id; return nil }
func (s *stubManager) RestartApplication(id string) error { s.restartCalled = id; return nil }
func (s *stubManager) RemoveApplication(id string) error {
	s.removeCalled = id
	return s.removeErr
}

func withStubManager(t *testing.T, stub *stubManager) {
	t.Helper()
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

		{name: "unknown strategy", payload: CommandPayload{Type: "rebuild", Strategy: "canary"}, wantErr: true},
		{name: "replicas on blue-green", payload: CommandPayload{Type: "rebuild", Strategy: "blue-green", Replicas: 2}, wantErr: true},
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
