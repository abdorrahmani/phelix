package monitor

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Tests for remote rollback command dispatch in the shared CommandExecutor:
// the "rollback" type must reach the registered handler verbatim (same
// service layer as the CLI), be rejected without one, and never fall through
// to the lifecycle/CLI-subprocess paths.

func captureRollbackHandler(t *testing.T, fn func(CommandPayload) error) {
	t.Helper()
	orig := RollbackHandler
	RollbackHandler = fn
	t.Cleanup(func() { RollbackHandler = orig })
}

func TestExecuteRollbackDispatchesToHandler(t *testing.T) {
	var got CommandPayload
	captureRollbackHandler(t, func(p CommandPayload) error {
		got = p
		return nil
	})

	err := NewCommandExecutor().Execute(Command{
		Type: "rollback",
		Payload: CommandPayload{
			Type:           "rollback",
			RequestID:      "req-7",
			AppName:        "shop",
			Target:         "v7",
			Reason:         "bad release",
			VerifyDuration: 30 * time.Second,
			DryRun:         true,
		},
	})
	if err != nil {
		t.Fatalf("Execute(rollback) returned error: %v", err)
	}
	if got != (CommandPayload{
		Type: "rollback", RequestID: "req-7", AppName: "shop",
		Target: "v7", Reason: "bad release", VerifyDuration: 30 * time.Second, DryRun: true,
	}) {
		t.Fatalf("payload not delivered verbatim: %+v", got)
	}
}

func TestExecuteRollbackWithoutHandlerRejected(t *testing.T) {
	captureRollbackHandler(t, nil)

	err := NewCommandExecutor().Execute(Command{
		Type:    "rollback",
		Payload: CommandPayload{Type: "rollback", AppName: "shop", Target: "v7"},
	})
	if !phelixerr.IsCode(err, phelixerr.CodeUnimplemented) {
		t.Fatalf("expected UNIMPLEMENTED without a handler, got %v", err)
	}
}

func TestExecuteRollbackHandlerErrorPropagates(t *testing.T) {
	want := phelixerr.New(phelixerr.CodeRollbackTargetNotFound, "no such version")
	captureRollbackHandler(t, func(CommandPayload) error { return want })

	err := NewCommandExecutor().Execute(Command{
		Type:    "rollback",
		Payload: CommandPayload{Type: "rollback", AppName: "shop", Target: "v99"},
	})
	if !errors.Is(err, want) {
		t.Fatalf("handler error not propagated: %v", err)
	}
}

// A rollback carrying deployment overrides is a backend bug: rejected
// deterministically, never silently ignored and never forwarded to the
// handler.
func TestExecuteRollbackRejectsDeploymentOverrides(t *testing.T) {
	var called bool
	captureRollbackHandler(t, func(CommandPayload) error { called = true; return nil })

	err := NewCommandExecutor().Execute(Command{
		Type:    "rollback",
		Payload: CommandPayload{Type: "rollback", AppName: "shop", Strategy: "rolling", Replicas: 2},
	})
	if !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("expected INVALID_ARGUMENT for overrides on rollback, got %v", err)
	}
	if called {
		t.Fatal("handler must not run for a rollback carrying overrides")
	}
}

// A rollback command must never reach the CLI-subprocess fallback, even with
// the handler absent — shelling out to `phelix rollback` is forbidden.
func TestExecuteRollbackNeverFallsBackToSubprocess(t *testing.T) {
	captureRollbackHandler(t, nil)
	ran := false
	orig := runPhelixCommand
	runPhelixCommand = func(*exec.Cmd) error { ran = true; return nil }
	t.Cleanup(func() { runPhelixCommand = orig })

	_ = NewCommandExecutor().Execute(Command{
		Type:    "rollback",
		Payload: CommandPayload{Type: "rollback", AppName: "shop", Target: "v7"},
	})
	if ran {
		t.Fatal("rollback command was executed as a `phelix` subprocess")
	}
}
