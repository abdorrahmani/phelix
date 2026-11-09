package security

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SchedulerPollInterval is how often the scheduler checks for due scans.
const SchedulerPollInterval = 60 * time.Second

// FailureBackoffBase is the first retry delay after a failed scheduled
// scan. It doubles per consecutive failure and is capped by the app's own
// scan interval, so failures never cause a tight retry loop.
const FailureBackoffBase = 15 * time.Minute

// ScheduledApp is one registered application as the scheduler sees it.
type ScheduledApp struct {
	AppID       string
	AppName     string
	IdentityKey string
	Dir         string
	// Enabled mirrors security.vulnerability.enabled: only enabled apps
	// are scheduled.
	Enabled bool
	// Interval is the resolved scan_interval for the app.
	Interval time.Duration
}

// Scheduler runs due vulnerability scans for enabled applications inside
// the monitor daemon. Cadence is completion-anchored: the next run is the
// last completed scan plus the configured interval, so intervals survive
// restarts and downtime never stacks up missed runs. Manual scans and
// scheduled scans coordinate through the same per-app lock.
type Scheduler struct {
	// Store persists scan records and the latest index.
	Store *ScanStore
	// PollInterval is how often due work is checked; zero uses the
	// default.
	PollInterval time.Duration
	// ListApps returns every registered application with its scan
	// settings. It is called once per poll tick.
	ListApps func() []ScheduledApp
	// Scan executes one scan and must honor ctx cancellation.
	Scan func(ctx context.Context, app ScheduledApp) error
	// Now makes the clock injectable for tests.
	Now func() time.Time
	// Log receives scheduler diagnostics. It must never receive
	// dependency inventories; only app names, scan ids, and counts.
	Log func(level string, format string, args ...any)

	failureCount map[string]int
	mu           sync.Mutex
}

// NewScheduler builds a scheduler over the given store.
func NewScheduler(store *ScanStore) *Scheduler {
	return &Scheduler{
		Store:        store,
		PollInterval: SchedulerPollInterval,
		Now:          time.Now,
		failureCount: map[string]int{},
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) log(level, format string, args ...any) {
	if s.Log != nil {
		s.Log(level, format, args...)
	}
}

// Run drives the scheduler until ctx is cancelled. On startup it marks
// abandoned running records interrupted; on shutdown it waits for the
// in-flight scan to observe cancellation and finish recording.
func (s *Scheduler) Run(ctx context.Context) {
	s.recoverInterrupted()
	interval := s.PollInterval
	if interval <= 0 {
		interval = SchedulerPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log("info", "security scheduler stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// recoverInterrupted closes out running records abandoned by a previous
// process. They are marked interrupted, never successful.
func (s *Scheduler) recoverInterrupted() {
	entries, err := os.ReadDir(s.Store.scansDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, err := s.Store.LoadScan(trimJSONSuffix(entry.Name()))
		if err != nil || record.Status != ScanRunning {
			continue
		}
		lock, err := s.Store.TryAcquireScanLock(record.IdentityKey)
		if errors.Is(err, ErrScanLockHeld) {
			continue
		}
		if err != nil {
			s.log("warning", "security: cannot check scan %s recovery: %v", record.ScanID, err)
			continue
		}
		err = s.Store.ReclaimInterrupted(record.IdentityKey)
		lock.Release()
		if err != nil {
			s.log("warning", "security: could not mark scan %s interrupted: %v", record.ScanID, err)
		}
	}
}

// dueApp decides whether one app is due and computes the basis the next
// schedule will use.
func (s *Scheduler) isDue(app ScheduledApp, now time.Time) bool {
	next, err := s.Store.NextScanTime(app.IdentityKey, app.Interval)
	if err != nil {
		s.log("warning", "security schedule unavailable for %s: %v", app.AppID, err)
		return false
	}
	return next == nil || !next.After(now)
}

// tick checks every app once. Scans execute sequentially inside the tick:
// one scan at a time is the concurrency bound, and a slow scan naturally
// delays the next poll instead of stacking work.
func (s *Scheduler) tick(ctx context.Context) {
	if s.ListApps == nil || s.Scan == nil {
		return
	}
	now := s.now()
	for _, app := range s.ListApps() {
		if err := ctx.Err(); err != nil {
			return
		}
		if !app.Enabled || app.IdentityKey == "" || app.Dir == "" {
			continue
		}
		if !s.isDue(app, now) {
			continue
		}
		if !s.tryScan(ctx, app, now) {
			continue
		}
	}
}

// tryScan runs one app's scan under the per-app lock and schedules the
// next run. A lock held by a manual scan skips this app until the next
// poll; the manual scan's completion time re-anchors the cadence.
func (s *Scheduler) tryScan(ctx context.Context, app ScheduledApp, now time.Time) bool {
	lock, err := s.Store.TryAcquireScanLock(app.IdentityKey)
	if err != nil {
		if errors.Is(err, ErrScanLockHeld) {
			s.log("info", "security: %s: a manual scan holds the scan lock; skipping this round", app.AppName)
			return false
		}
		s.log("warning", "security: %s: could not take the scan lock: %v", app.AppName, err)
		return false
	}
	defer lock.Release()

	err = s.Scan(ctx, app)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		backoff := s.nextBackoff(app.IdentityKey, app.Interval)
		if scheduleErr := s.Store.ScheduleNext(app.IdentityKey, now.Add(backoff), app.Interval); scheduleErr != nil {
			s.log("warning", "security: %s: could not schedule a retry: %v", app.AppName, scheduleErr)
		}
		s.log("warning", "security: scheduled scan of %s failed (%s); retrying in %s", app.AppName, err, backoff)
		return true
	}
	s.mu.Lock()
	s.failureCount[app.IdentityKey] = 0
	s.mu.Unlock()

	entry, entryErr := s.Store.Latest(app.IdentityKey)
	finished := s.now()
	if entryErr == nil && entry != nil && entry.FinishedAt != nil {
		finished = *entry.FinishedAt
	}
	if scheduleErr := s.Store.ScheduleNext(app.IdentityKey, finished.Add(app.Interval), app.Interval); scheduleErr != nil {
		s.log("warning", "security: %s: could not schedule the next scan: %v", app.AppName, scheduleErr)
	}
	s.log("info", "security: scheduled scan of %s completed (scan %s); next run at %s", app.AppName, entryScanID(entry), finished.Add(app.Interval).Format(time.RFC3339))
	return true
}

// nextBackoff doubles per consecutive failure, capped by the interval.
func (s *Scheduler) nextBackoff(identityKey string, interval time.Duration) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failureCount[identityKey]++
	backoff := FailureBackoffBase
	for i := 1; i < s.failureCount[identityKey] && backoff < interval; i++ {
		backoff *= 2
	}
	if backoff > interval {
		backoff = interval
	}
	return backoff
}

func entryScanID(entry *LatestEntry) string {
	if entry == nil {
		return ""
	}
	return entry.ScanID
}

func trimJSONSuffix(name string) string {
	if len(name) > 5 && name[len(name)-5:] == ".json" {
		return name[:len(name)-5]
	}
	return name
}
