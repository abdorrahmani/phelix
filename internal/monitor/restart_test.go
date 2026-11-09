package monitor

import (
	"errors"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestValidateRestartMonitorRequest(t *testing.T) {
	if err := ValidateRestartMonitorRequest("", "req-1"); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if err := ValidateRestartMonitorRequest("app", "req-1"); err == nil {
		t.Fatal("app-targeted restart accepted")
	}
	if err := ValidateRestartMonitorRequest("", ""); err == nil {
		t.Fatal("request without id accepted")
	}
}

func TestRestartMonitorUnavailableFailsClosed(t *testing.T) {
	previous := RestartAgentHandler
	RestartAgentHandler = nil
	t.Cleanup(func() { RestartAgentHandler = previous })

	err := RestartMonitor()
	if !phelixerr.IsCode(err, phelixerr.CodeUnimplemented) {
		t.Fatalf("error = %v, want CodeUnimplemented", err)
	}
}

func TestRestartRequestReplayIsNotExecutedTwice(t *testing.T) {
	outcome, seen := BeginRestartRequest("req-replay")
	if seen {
		t.Fatal("first request reported as seen")
	}
	FinishRestartRequest("req-replay", errors.New("boom"))

	replay, seen := BeginRestartRequest("req-replay")
	if !seen {
		t.Fatal("replayed request not detected")
	}
	if replay.ErrorText == "" || outcome.ErrorText != "" {
		t.Fatalf("unexpected outcomes: first=%+v replay=%+v", outcome, replay)
	}
}
