package hostsecurity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

type hostSchedulerFixture struct {
	scheduler *Scheduler
	store     *Store
	dataDir   string
	base      time.Time
	scanCalls int
	scanErr   error
	logs      []string
}

func newHostSchedulerFixture(t *testing.T) *hostSchedulerFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &hostSchedulerFixture{store: store, dataDir: dataDir, base: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	if err := SaveHostConfig(dataDir, HostConfig{Reporting: true, ScanInterval: DefaultHostScanIntervalValue}); err != nil {
		t.Fatal(err)
	}
	f.scheduler = NewScheduler(store, dataDir, "srv")
	f.scheduler.PollInterval = time.Millisecond
	f.scheduler.Now = func() time.Time { return f.base }
	f.scheduler.Scan = func(ctx context.Context) error {
		f.scanCalls++
		return f.scanErr
	}
	f.scheduler.Log = func(level string, format string, args ...any) {
		f.logs = append(f.logs, level+": "+fmt.Sprintf(format, args...))
	}
	return f
}

func (f *hostSchedulerFixture) tick(t *testing.T) {
	t.Helper()
	f.scheduler.tick(context.Background())
}

func (f *hostSchedulerFixture) schedule() hostSchedule {
	state, err := loadHostSchedule(f.dataDir)
	if err != nil {
		return hostSchedule{}
	}
	return state
}

// saveFinishedScan persists one completed scan record anchored at the
// fixture's base clock minus the given age.
func (f *hostSchedulerFixture) saveFinishedScan(t *testing.T, id string, age time.Duration) {
	t.Helper()
	scan := Scan{ID: id, ServerID: "srv", Domain: "server", Status: "completed", StartedAt: f.base.Add(-age - time.Minute), FinishedAt: f.base.Add(-age), PackageCount: 0, SnapshotID: "snap-" + id, Coverage: "complete", Warnings: []string{}}
	if err := f.store.SaveScan(context.Background(), scan, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHostScheduler_ScansDueHostAndSchedulesNext(t *testing.T) {
	f := newHostSchedulerFixture(t)
	f.tick(t)
	if f.scanCalls != 1 {
		t.Fatalf("a never-scanned consented host must be due: %d", f.scanCalls)
	}
	state := f.schedule()
	if state.Running || state.NextRunAt != "" || state.SuccessFinished != f.base.Format(time.RFC3339) {
		t.Fatalf("completion anchor not recorded: %+v", state)
	}
	f.tick(t)
	if f.scanCalls != 1 {
		t.Fatalf("a host before its next run must not rescan: %d", f.scanCalls)
	}
	f.base = f.base.Add(DefaultHostScanInterval)
	f.tick(t)
	if f.scanCalls != 2 {
		t.Fatalf("the host must rescan after one interval: %d", f.scanCalls)
	}
}

func TestHostScheduler_AnchorsOnStoredCompletion(t *testing.T) {
	f := newHostSchedulerFixture(t)
	f.saveFinishedScan(t, "s1", time.Hour)
	f.tick(t)
	if f.scanCalls != 0 {
		t.Fatalf("a scan finished an hour ago is not due: %d", f.scanCalls)
	}
	f.base = f.base.Add(DefaultHostScanInterval - time.Hour)
	f.tick(t)
	if f.scanCalls != 1 {
		t.Fatalf("the cadence must be completion anchored: %d", f.scanCalls)
	}
}

func TestHostScheduler_ConsentGate(t *testing.T) {
	f := newHostSchedulerFixture(t)
	if err := SaveHostConfig(f.dataDir, HostConfig{Reporting: false, ScanInterval: DefaultHostScanIntervalValue}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if f.scanCalls != 0 {
		t.Fatalf("disabled consent must skip scheduled scans: %d", f.scanCalls)
	}
	if err := SaveHostConfig(f.dataDir, HostConfig{Reporting: true, ScanInterval: DefaultHostScanIntervalValue}); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if f.scanCalls != 1 {
		t.Fatalf("enabled consent must schedule: %d", f.scanCalls)
	}
	if err := os.WriteFile(HostConfigPath(f.dataDir), []byte(`{"reporting":true,"scan_interval":"broken"}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.base = f.base.Add(DefaultHostScanInterval)
	f.tick(t)
	if f.scanCalls != 1 {
		t.Fatalf("an invalid consent file must fail closed: %d", f.scanCalls)
	}
}

func TestHostScheduler_BackoffDoublesCappedAtInterval(t *testing.T) {
	f := newHostSchedulerFixture(t)
	if err := SaveHostConfig(f.dataDir, HostConfig{Reporting: true, ScanInterval: "1h"}); err != nil {
		t.Fatal(err)
	}
	f.scanErr = errors.New("inventory unavailable")
	f.tick(t)
	if f.scanCalls != 1 || f.schedule().NextRunAt != f.base.Add(15*time.Minute).Format(time.RFC3339) {
		t.Fatalf("first failure must back off 15m: %d %+v", f.scanCalls, f.schedule())
	}
	f.base = f.base.Add(14 * time.Minute)
	f.tick(t)
	if f.scanCalls != 1 {
		t.Fatalf("must wait out the backoff: %d", f.scanCalls)
	}
	f.base = f.base.Add(time.Minute)
	f.tick(t)
	if f.scanCalls != 2 || f.schedule().NextRunAt != f.base.Add(30*time.Minute).Format(time.RFC3339) {
		t.Fatalf("second failure must back off 30m: %d %+v", f.scanCalls, f.schedule())
	}
	f.base = f.base.Add(30 * time.Minute)
	f.tick(t)
	if f.scanCalls != 3 {
		t.Fatalf("third attempt must run: %d", f.scanCalls)
	}
	if f.schedule().NextRunAt != f.base.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("backoff must cap at the interval: %+v", f.schedule())
	}
	f.scanErr = nil
	f.base = f.base.Add(time.Hour)
	f.tick(t)
	if f.scanCalls != 4 {
		t.Fatalf("the retry must run: %d", f.scanCalls)
	}
	if f.schedule().NextRunAt != "" || f.schedule().Failures != 0 {
		t.Fatalf("success must clear the failure state: %+v", f.schedule())
	}
}

func TestHostScheduler_ManualLockSkipsRound(t *testing.T) {
	f := newHostSchedulerFixture(t)
	lock, err := TryAcquireHostScanLock(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if f.scanCalls != 0 {
		t.Fatalf("a held scan lock must skip the round: %d", f.scanCalls)
	}
	lock.Release()
	f.tick(t)
	if f.scanCalls != 1 {
		t.Fatalf("releasing the lock must unblock the schedule: %d", f.scanCalls)
	}
}

func TestHostScheduler_ReclaimsAbandonedRun(t *testing.T) {
	f := newHostSchedulerFixture(t)
	if err := saveHostSchedule(f.dataDir, hostSchedule{Running: true}); err != nil {
		t.Fatal(err)
	}
	f.scheduler.recoverAbandoned()
	if f.schedule().Running {
		t.Fatal("an abandoned run marker must be reclaimed")
	}

	if err := saveHostSchedule(f.dataDir, hostSchedule{Running: true}); err != nil {
		t.Fatal(err)
	}
	lock, err := TryAcquireHostScanLock(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	f.scheduler.recoverAbandoned()
	if !f.schedule().Running {
		t.Fatal("a live scan's marker must not be reclaimed")
	}
}

func TestHostScheduler_RecoverMarksAbandonedOnStartup(t *testing.T) {
	f := newHostSchedulerFixture(t)
	if err := saveHostSchedule(f.dataDir, hostSchedule{Running: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.scheduler.Run(ctx)
	if f.schedule().Running {
		t.Fatal("Run startup must reclaim the abandoned marker")
	}
}

func TestHostScheduler_UnreadableScheduleSkipsRound(t *testing.T) {
	f := newHostSchedulerFixture(t)
	corrupt := []byte("{not json at all")
	if err := os.WriteFile(hostSchedulePath(f.dataDir), corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if f.scanCalls != 0 {
		t.Fatalf("an unreadable schedule file must skip the round: %d", f.scanCalls)
	}
	raw, err := os.ReadFile(hostSchedulePath(f.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(corrupt) {
		t.Fatalf("the skipped round must not overwrite the schedule file with zeros: %q", raw)
	}
}

func TestHostScheduler_PostScanReloadFailurePreservesBackoff(t *testing.T) {
	f := newHostSchedulerFixture(t)
	if err := SaveHostConfig(f.dataDir, HostConfig{Reporting: true, ScanInterval: "1h"}); err != nil {
		t.Fatal(err)
	}
	past := f.base.Add(-time.Minute).Format(time.RFC3339)
	if err := saveHostSchedule(f.dataDir, hostSchedule{Failures: 3, NextRunAt: past}); err != nil {
		t.Fatal(err)
	}
	f.scanErr = errors.New("inventory unavailable")
	previous := f.scheduler.Scan
	f.scheduler.Scan = func(ctx context.Context) error {
		// The scan corrupts the schedule file before failing, so the
		// post-scan reload cannot succeed.
		if err := os.WriteFile(hostSchedulePath(f.dataDir), []byte("garbage"), 0600); err != nil {
			t.Error(err)
		}
		return previous(ctx)
	}
	f.tick(t)
	if f.scanCalls != 1 {
		t.Fatalf("a due round must run: %d", f.scanCalls)
	}
	state := f.schedule()
	if state.Failures != 4 {
		t.Fatalf("the preserved failure count must increment: %+v", state)
	}
	if state.NextRunAt != f.base.Add(15*time.Minute).Format(time.RFC3339) {
		t.Fatalf("the retry anchor must survive the reload failure: %+v", state)
	}
	if state.Running {
		t.Fatalf("the in-flight marker must be cleared: %+v", state)
	}
}

func TestHostScanLockSerialization(t *testing.T) {
	dataDir := t.TempDir()
	if got, want := hostScanLockPath(dataDir), dataDir+"/host-security/scan.lock"; got != want {
		t.Fatalf("lock path %s, want %s", got, want)
	}
	lock, err := TryAcquireHostScanLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryAcquireHostScanLock(dataDir); !errors.Is(err, ErrHostScanLockHeld) {
		t.Fatalf("second try must report a held lock: %v", err)
	}
	lock.Release()
	lock, err = AcquireHostScanLock(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
}
