package monitor

import (
	"errors"
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
