package job

import (
	"context"
	"testing"
	"time"
)

func schedulerFor(t *testing.T, clock *time.Time) (*Scheduler, *Executor, string) {
	t.Helper()
	executor, dir := testExecutor(t)
	executor.Now = func() time.Time { return *clock }
	scheduler := NewScheduler(executor)
	scheduler.Now = func() time.Time { return *clock }
	scheduler.ListJobs = func() []ScheduledJob {
		return []ScheduledJob{{
			AppName:  "api",
			Dir:      dir,
			Cron:     "* * * * *",
			Location: time.UTC,
			Command:  []string{"/bin/sh", "-c", "true"},
		}}
	}
	return scheduler, executor, dir
}

func TestSchedulerRunsDueOccurrence(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	scheduler, executor, _ := schedulerFor(t, &clock)
	scheduler.tick(context.Background())
	if list, _ := executor.Store.List("api", 0); len(list) != 0 {
		t.Fatalf("ran before the occurrence was due: %+v", list)
	}
	clock = time.Date(2026, 1, 1, 12, 1, 0, 0, time.UTC)
	scheduler.tick(context.Background())
	list, err := executor.Store.List("api", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("records = %d, want 1", len(list))
	}
	if list[0].Status != StatusSucceeded || list[0].Trigger != TriggerScheduled {
		t.Errorf("record = %+v", list[0])
	}
	if list[0].ScheduledFor == nil || !list[0].ScheduledFor.Equal(clock) {
		t.Errorf("scheduled_for = %v, want %s", list[0].ScheduledFor, clock)
	}
}

func TestSchedulerSkipsMissedOccurrencesOnStartup(t *testing.T) {
	clock := time.Date(2026, 1, 1, 15, 30, 0, 0, time.UTC)
	scheduler, executor, _ := schedulerFor(t, &clock)
	scheduler.tick(context.Background())
	list, err := executor.Store.List("api", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("startup replayed %d missed occurrence(s)", len(list))
	}
	scheduler.mu.Lock()
	next := scheduler.states["api"].next
	scheduler.mu.Unlock()
	if next.IsZero() || !next.After(clock) {
		t.Errorf("next = %s, want a future occurrence", next)
	}
}

func TestSchedulerDoesNotRepeatRecordedOccurrence(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	scheduler, executor, dir := schedulerFor(t, &clock)
	when := time.Date(2026, 1, 1, 12, 1, 0, 0, time.UTC)
	record, err := executor.Skip(RunRequest{AppName: "api", Dir: dir, Command: []string{"true"}}, when, "already ran")
	if err != nil {
		t.Fatalf("Skip: %v", err)
	}
	scheduler.tick(context.Background())
	clock = when
	scheduler.tick(context.Background())
	list, err := executor.Store.List("api", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != record.ID {
		t.Fatalf("records = %+v, want only the existing occurrence", list)
	}
}

func TestSchedulerRecordsSkippedConcurrency(t *testing.T) {
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	scheduler, executor, _ := schedulerFor(t, &clock)
	lock, err := executor.Store.TryAcquireRunLock("api")
	if err != nil {
		t.Fatalf("TryAcquireRunLock: %v", err)
	}
	defer lock.Release()
	scheduler.tick(context.Background())
	clock = time.Date(2026, 1, 1, 12, 1, 0, 0, time.UTC)
	scheduler.tick(context.Background())
	list, err := executor.Store.List("api", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Status != StatusSkipped {
		t.Fatalf("records = %+v, want one skipped occurrence", list)
	}
}
