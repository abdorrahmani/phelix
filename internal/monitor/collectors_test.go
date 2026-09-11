package monitor

import "testing"

func TestNewMetricsCollector_ReturnsUsableImplementation(t *testing.T) {
	c := NewMetricsCollector()
	if c == nil {
		t.Fatal("expected non-nil MetricsCollector")
	}

	// These must not panic even with no managed apps / no server
	// initialized; they should return empty results or a handled error.
	_ = c.CollectAppMetrics()
	_ = c.CollectAppDetails()

	if _, err := c.CollectAppLogs(); err != nil {
		t.Logf("CollectAppLogs returned error (acceptable in test env): %v", err)
	}
	if _, err := c.CollectSelfLogs(); err != nil {
		t.Logf("CollectSelfLogs returned error (acceptable in test env): %v", err)
	}
}

func TestNewCommandExecutor_ReturnsUsableImplementation(t *testing.T) {
	e := NewCommandExecutor()
	if e == nil {
		t.Fatal("expected non-nil CommandExecutor")
	}

	// Executing a command for a nonexistent app must return an error, not
	// panic.
	err := e.Execute(Command{
		Type: "restart",
		Payload: CommandPayload{
			Type:    "restart",
			AppName: "definitely-does-not-exist",
		},
	})
	if err == nil {
		t.Fatal("expected error executing command for nonexistent app")
	}
}
