package security

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type schedulerFixture struct {
	scheduler *Scheduler
	store     *ScanStore
	scans     []ScheduledApp
	scanCalls []string
	logs      []string
	base      time.Time
}

func newSchedulerFixture(t *testing.T) *schedulerFixture {
	t.Helper()
	f := &schedulerFixture{
		store: NewScanStore(t.TempDir()),
		base:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}
	f.scheduler = NewScheduler(f.store)
	f.scheduler.PollInterval = time.Millisecond
	f.scheduler.Now = func() time.Time { return f.base }
	f.scheduler.ListApps = func() []ScheduledApp { return f.scans }
	f.scheduler.Scan = func(ctx context.Context, app ScheduledApp) error {
		f.scanCalls = append(f.scanCalls, app.IdentityKey)
		return nil
	}
	f.scheduler.Log = func(level, format string, args ...any) {
		f.logs = append(f.logs, level+": "+format)
	}
	return f
}

func (f *schedulerFixture) tick(t *testing.T) {
	t.Helper()
	f.scheduler.tick(context.Background())
}

func (f *schedulerFixture) app(t *testing.T, name string, enabled bool) ScheduledApp {
	t.Helper()
	return ScheduledApp{
		AppID:       "11111111-1111-4111-8111-11111111111" + name[len(name)-1:],
		AppName:     name,
		IdentityKey: "name:" + name,
		Dir:         t.TempDir(),
		Enabled:     enabled,
		Interval:    time.Hour,
	}
}

func TestScheduler_ScansDueAppAndSchedulesNext(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "myapp", true)
	f.scans = []ScheduledApp{app}

	f.tick(t)
	if len(f.scanCalls) != 1 || f.scanCalls[0] != app.IdentityKey {
		t.Fatalf("a never-scanned enabled app must be due: %v", f.scanCalls)
	}
	entry, err := f.store.Latest(app.IdentityKey)
	if err != nil || entry == nil || entry.NextScanAt == nil {
		t.Fatalf("next run must be scheduled: %v", entry)
	}
	if !entry.NextScanAt.Equal(f.base.Add(app.Interval)) {
		t.Fatalf("cadence is completion anchored: next=%v want=%v", entry.NextScanAt, f.base.Add(app.Interval))
	}

	f.tick(t)
	if len(f.scanCalls) != 1 {
		t.Fatalf("an app before its next run must not rescan: %v", f.scanCalls)
	}
}

func TestScheduler_SkipsDisabledApps(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "offapp", false)
	f.scans = []ScheduledApp{app}

	f.tick(t)
	if len(f.scanCalls) != 0 {
		t.Fatalf("disabled apps must never be scheduled: %v", f.scanCalls)
	}
}

func TestScheduler_MissedRunRunsOnceAfterDowntime(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "myapp", true)
	f.scans = []ScheduledApp{app}
	if err := f.store.ScheduleNext(app.IdentityKey, f.base.Add(-24*time.Hour), app.Interval); err != nil {
		t.Fatal(err)
	}

	f.tick(t)
	if len(f.scanCalls) != 1 {
		t.Fatalf("an overdue app must run at the next tick: %v", f.scanCalls)
	}
	entry, _ := f.store.Latest(app.IdentityKey)
	if !entry.NextScanAt.Equal(f.base.Add(app.Interval)) {
		t.Fatalf("downtime must not stack missed runs: %v", entry.NextScanAt)
	}
}

func TestScheduler_ManualLockSkipsRound(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "myapp", true)
	f.scans = []ScheduledApp{app}

	held, err := f.store.AcquireScanLock(app.IdentityKey)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	f.tick(t)
	if len(f.scanCalls) != 0 {
		t.Fatalf("a manual scan holding the lock must delay the scheduled run: %v", f.scanCalls)
	}

	held.Release()
	f.tick(t)
	if len(f.scanCalls) != 1 {
		t.Fatalf("the scheduled run must proceed once the lock frees: %v", f.scanCalls)
	}
}

func TestScheduler_FailureBackoff(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "myapp", true)
	f.scans = []ScheduledApp{app}
	f.scheduler.Scan = func(ctx context.Context, app ScheduledApp) error {
		f.scanCalls = append(f.scanCalls, app.IdentityKey)
		return errors.New("network unreachable")
	}

	f.tick(t)
	entry, _ := f.store.Latest(app.IdentityKey)
	if !entry.NextScanAt.Equal(f.base.Add(FailureBackoffBase)) {
		t.Fatalf("first failure must back off by the base delay: %v", entry.NextScanAt)
	}

	f.base = f.base.Add(FailureBackoffBase)
	f.tick(t)
	entry, _ = f.store.Latest(app.IdentityKey)
	if !entry.NextScanAt.Equal(f.base.Add(2 * FailureBackoffBase)) {
		t.Fatalf("second failure must double the backoff: %v", entry.NextScanAt)
	}

	f.scheduler.Scan = func(ctx context.Context, app ScheduledApp) error {
		f.scanCalls = append(f.scanCalls, app.IdentityKey)
		return nil
	}
	f.base = f.base.Add(2 * FailureBackoffBase)
	f.tick(t)
	entry, _ = f.store.Latest(app.IdentityKey)
	if !entry.NextScanAt.Equal(f.base.Add(app.Interval)) {
		t.Fatalf("success must re-anchor the cadence: %v", entry.NextScanAt)
	}
	f.tick(t)
	if len(f.scanCalls) != 3 {
		t.Fatalf("a successful scan resets the cadence: %v", f.scanCalls)
	}
}

func TestScheduler_BackoffCappedByInterval(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "myapp", true)
	app.Interval = 20 * time.Minute
	f.scans = []ScheduledApp{app}
	f.scheduler.Scan = func(ctx context.Context, app ScheduledApp) error { return errors.New("down") }

	for i := 0; i < 5; i++ {
		f.tick(t)
		entry, _ := f.store.Latest(app.IdentityKey)
		f.base = *entry.NextScanAt
	}
	entry, _ := f.store.Latest(app.IdentityKey)
	if !entry.NextScanAt.Equal(f.base) {
		t.Fatalf("backoff must converge on the schedule: %v", entry.NextScanAt)
	}
}

func TestScheduler_RecoversAbandonedRuns(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "myapp", true)
	f.scans = []ScheduledApp{app}

	abandoned := f.base.Add(-2 * time.Hour)
	if err := f.store.SaveScan(&ScanRecord{
		ScanID:      NewScanID(),
		IdentityKey: app.IdentityKey,
		AppName:     app.AppName,
		Status:      ScanRunning,
		StartedAt:   abandoned,
	}); err != nil {
		t.Fatal(err)
	}
	live := &ScanRecord{
		ScanID:      NewScanID(),
		IdentityKey: "name:other",
		AppName:     "other",
		Status:      ScanRunning,
		StartedAt:   f.base.Add(-time.Minute),
	}
	if err := f.store.SaveScan(live); err != nil {
		t.Fatal(err)
	}

	f.scheduler.recoverInterrupted()
	records, _ := f.store.ListScans(app.IdentityKey)
	if len(records) != 1 || records[0].Status != ScanInterrupted {
		t.Fatalf("an abandoned running record must be marked interrupted: %+v", records)
	}
	if records[0].Outcome != "interrupted by a process restart" {
		t.Fatalf("interrupted records must say why: %s", records[0].Outcome)
	}
	other, _ := f.store.ListScans("name:other")
	if len(other) != 1 || other[0].Status != ScanRunning {
		t.Fatalf("a recent running record belongs to a live process: %+v", other)
	}
	_ = live
}

func TestScheduler_ShutdownCancelsInFlightScan(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "myapp", true)
	f.scans = []ScheduledApp{app}

	scanCtxAlive := make(chan struct{})
	scanDone := make(chan struct{})
	f.scheduler.Scan = func(ctx context.Context, app ScheduledApp) error {
		close(scanCtxAlive)
		<-ctx.Done()
		close(scanDone)
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.scheduler.Run(ctx)
		close(done)
	}()

	<-scanCtxAlive
	cancel()
	select {
	case <-scanDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the in-flight scan must observe cancellation")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduler must exit after the scan settles")
	}
}

func TestScheduler_ScheduledScanRecordsTrigger(t *testing.T) {
	f := newSchedulerFixture(t)
	app := f.app(t, "myapp", true)
	f.scans = []ScheduledApp{app}

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	server := fakeOSV(t, EcosystemGo, "example.com/quiet", "GO-2024-4001",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "9.9.9")}})
	defer server.Close()

	f.scheduler.Scan = func(ctx context.Context, sched ScheduledApp) error {
		opts := ScanOptions{
			AppName:     sched.AppName,
			IdentityKey: sched.IdentityKey,
			Dir:         projectDir,
			Trigger:     TriggerScheduled,
			DataDir:     dataDir,
			OSVBaseURL:  server.URL,
		}
		if _, err := RunScan(ctx, opts); err != nil {
			t.Fatalf("scheduled scan: %v", err)
		}
		f.scanCalls = append(f.scanCalls, sched.IdentityKey)
		return nil
	}

	f.tick(t)
	store := NewScanStore(dataDir)
	records, _ := store.ListScans(app.IdentityKey)
	if len(records) != 1 || records[0].Trigger != TriggerScheduled {
		t.Fatalf("scheduled scans must record their trigger: %+v", records)
	}
	if records[0].Status != ScanCompleted {
		t.Fatalf("the scheduled scan must complete: %s", records[0].Status)
	}
}
