package job

import (
	"context"
	"fmt"
	"testing"
)

// TestExecutorEmitsLifecycleEvents verifies the documented phase order for a
// successful run and that every event carries the execution identity.
func TestExecutorEmitsLifecycleEvents(t *testing.T) {
	executor, dir := testExecutor(t)
	var events []Event
	executor.Store.Events = func(event Event) { events = append(events, event) }

	record, err := executor.Run(context.Background(), RunRequest{
		AppName: "api",
		AppID:   "app-1",
		Dir:     dir,
		Command: []string{"/bin/sh", "-c", "exit 0"},
		Trigger: TriggerManual,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []Phase{PhaseAccepted, PhaseStarted, PhaseAttemptStarted, PhaseSucceeded}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want phases %v", events, want)
	}
	for i, phase := range want {
		if events[i].Phase != phase {
			t.Fatalf("events[%d].Phase = %q, want %q", i, events[i].Phase, phase)
		}
		if events[i].ExecutionID != record.ID || events[i].AppName != "api" || events[i].AppID != "app-1" {
			t.Fatalf("events[%d] identity = %+v", i, events[i])
		}
		if events[i].Trigger != TriggerManual {
			t.Fatalf("events[%d].Trigger = %q", i, events[i].Trigger)
		}
	}
	if events[len(events)-1].Status != StatusSucceeded {
		t.Fatalf("final status = %q, want %q", events[len(events)-1].Status, StatusSucceeded)
	}
}

// TestExecutorEmitsAttemptFailurePerRetry locks the retry visibility: each
// failed attempt is reported before the final status.
func TestExecutorEmitsAttemptFailurePerRetry(t *testing.T) {
	executor, dir := testExecutor(t)
	var phases []Phase
	executor.Store.Events = func(event Event) { phases = append(phases, event.Phase) }

	record, err := executor.Run(context.Background(), RunRequest{
		AppName: "api",
		Dir:     dir,
		Command: []string{"/bin/sh", "-c", "exit 3"},
		Retries: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if record.Status != StatusFailed {
		t.Fatalf("status = %q", record.Status)
	}

	want := fmt.Sprint([]Phase{
		PhaseAccepted, PhaseStarted, PhaseAttemptStarted, PhaseAttemptFailed,
		PhaseAttemptStarted, PhaseAttemptFailed, PhaseFailed,
	})
	if got := fmt.Sprint(phases); got != want {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
}

// TestSkipEvent verifies skipped occurrences are observable.
func TestSkipEvent(t *testing.T) {
	executor, dir := testExecutor(t)
	var events []Event
	executor.Store.Events = func(event Event) { events = append(events, event) }

	record, err := executor.Skip(RunRequest{AppName: "api", Dir: dir}, executor.Now(), "lock held")
	if err != nil {
		t.Fatalf("Skip: %v", err)
	}
	if len(events) != 1 || events[0].Phase != PhaseSkipped || events[0].Status != StatusSkipped {
		t.Fatalf("events = %+v", events)
	}
	if events[0].ExecutionID != record.ID || events[0].Error != "lock held" {
		t.Fatalf("event = %+v", events[0])
	}
}

// TestRecoverInterruptedEvent verifies recovered executions announce the
// interrupted phase after the record is durable.
func TestRecoverInterruptedEvent(t *testing.T) {
	store := testStore(t)
	var events []Event
	store.Events = func(event Event) { events = append(events, event) }

	record := testRecord("api", NewID(), StatusRunning, store.Now())
	record.PID = 999999
	if err := store.Save(record); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := store.RecoverInterrupted(); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if len(events) != 1 || events[0].Phase != PhaseInterrupted || events[0].Status != StatusInterrupted {
		t.Fatalf("events = %+v", events)
	}
	if events[0].ExecutionID != record.ID {
		t.Fatalf("execution id = %q, want %q", events[0].ExecutionID, record.ID)
	}
}
