package hostsecurity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SchedulerPollInterval is how often the host scheduler checks for due
// scans.
const SchedulerPollInterval = 60 * time.Second

// FailureBackoffBase is the first retry delay after a failed scheduled host
// scan. It doubles per consecutive failure and is capped by the consented
// scan interval, so failures never cause a tight retry loop.
const FailureBackoffBase = 15 * time.Minute

// ErrHostScanLockHeld reports that another process holds the host scan
// lock. Scheduling policy treats it as "skip this round", never as a
// failure.
var ErrHostScanLockHeld = errors.New("host scan lock is held by another process")

// hostSchedule is the persisted schedule state for the recurring host scan:
// the pending failure retry anchor, the completion anchor fallback for
// stores without a finished record, and the in-flight marker reclaimed on
// startup when a scheduler died mid-scan.
type hostSchedule struct {
	NextRunAt       string `json:"next_run_at,omitempty"`
	SuccessFinished string `json:"success_finished_at,omitempty"`
	Failures        int    `json:"failures,omitempty"`
	Running         bool   `json:"running,omitempty"`
}

// HostScanLock exclusively locks this host for the duration of one scan.
type HostScanLock struct{ file *os.File }

func hostScanLockPath(dataDir string) string {
	return filepath.Join(dataDir, "host-security", "scan.lock")
}

func hostSchedulePath(dataDir string) string {
	return filepath.Join(dataDir, "host-security", "schedule.json")
}

// AcquireHostScanLock takes the exclusive host scan lock, blocking until it
// is available. Manual and scheduled scans coordinate through this lock.
func AcquireHostScanLock(dataDir string) (*HostScanLock, error) {
	if err := os.MkdirAll(filepath.Dir(hostScanLockPath(dataDir)), 0700); err != nil {
		return nil, err
	}
	f, err := acquireHostLock(hostScanLockPath(dataDir), true)
	if err != nil {
		return nil, err
	}
	return &HostScanLock{file: f}, nil
}

// TryAcquireHostScanLock takes the host scan lock without blocking. A held
// lock reports ErrHostScanLockHeld.
func TryAcquireHostScanLock(dataDir string) (*HostScanLock, error) {
	if err := os.MkdirAll(filepath.Dir(hostScanLockPath(dataDir)), 0700); err != nil {
		return nil, err
	}
	f, err := acquireHostLock(hostScanLockPath(dataDir), false)
	if err != nil {
		return nil, err
	}
	return &HostScanLock{file: f}, nil
}

// Release drops the host scan lock.
func (l *HostScanLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = releaseHostLock(l.file)
	_ = l.file.Close()
	l.file = nil
}

func loadHostSchedule(dataDir string) (hostSchedule, error) {
	var state hostSchedule
	raw, err := os.ReadFile(hostSchedulePath(dataDir))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, errors.New("host security schedule state is malformed")
	}
	return state, nil
}

func saveHostSchedule(dataDir string, state hostSchedule) error {
	path := hostSchedulePath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "schedule-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(name, 0600)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Scheduler runs due host security scans inside the monitor daemon. Cadence
// is completion-anchored: the next run is the latest finished scan plus the
// consented scan interval, so intervals survive restarts and downtime never
// stacks up missed runs. Scheduled scans are skipped entirely while
// reporting consent is disabled, and a manual scan holding the host scan
// lock defers the round to the next poll.
type Scheduler struct {
	// Store persists immutable scan records used for the completion anchor.
	Store *Store
	// DataDir holds the consent configuration, schedule state, and lock.
	DataDir string
	// ServerID scopes the completion anchor to this agent's scan records.
	ServerID string
	// PollInterval is how often due work is checked; zero uses the default.
	PollInterval time.Duration
	// Scan executes one scan and must honor ctx cancellation. It runs with
	// the host scan lock held.
	Scan func(ctx context.Context) error
	// Now makes the clock injectable for tests.
	Now func() time.Time
	// Log receives scheduler diagnostics; never package or advisory detail.
	Log func(level string, format string, args ...any)

	mu       sync.Mutex
	failures int
}

// NewScheduler builds a host scheduler over the given store.
func NewScheduler(store *Store, dataDir, serverID string) *Scheduler {
	return &Scheduler{Store: store, DataDir: dataDir, ServerID: serverID, PollInterval: SchedulerPollInterval, Now: time.Now}
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

// Run drives the scheduler until ctx is cancelled. On startup it reclaims
// the marker of a scheduled scan whose process died mid-scan.
func (s *Scheduler) Run(ctx context.Context) {
	s.recoverAbandoned()
	interval := s.PollInterval
	if interval <= 0 {
		interval = SchedulerPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log("info", "host security scheduler stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// recoverAbandoned reclaims the in-flight marker of a scheduled scan whose
// process died: the kernel has released its flock by then, so a free lock
// proves the run was abandoned and the marker must not linger.
func (s *Scheduler) recoverAbandoned() {
	lock, err := TryAcquireHostScanLock(s.DataDir)
	if errors.Is(err, ErrHostScanLockHeld) {
		return
	}
	if err != nil {
		s.log("warning", "host security: cannot check schedule recovery: %v", err)
		return
	}
	defer lock.Release()
	state, err := loadHostSchedule(s.DataDir)
	if err != nil || !state.Running {
		return
	}
	state.Running = false
	if err := saveHostSchedule(s.DataDir, state); err != nil {
		s.log("warning", "host security: could not reclaim the abandoned scan marker: %v", err)
		return
	}
	s.log("warning", "host security: reclaimed an abandoned scheduled scan")
}

// nextRun resolves the completion-anchored schedule: the latest finished
// scan plus the consented interval, dominated by a pending failure retry,
// and falling back to the persisted completion anchor when the store has no
// finished record yet.
func (s *Scheduler) nextRun(ctx context.Context, interval time.Duration) time.Time {
	var next time.Time
	state, err := loadHostSchedule(s.DataDir)
	if err != nil {
		s.log("warning", "host security schedule state unreadable: %v", err)
	}
	latest, err := s.Store.LatestFinished(ctx, s.ServerID)
	if err == nil && !latest.FinishedAt.IsZero() {
		next = latest.FinishedAt.Add(interval)
	} else if state.SuccessFinished != "" {
		if finished, e := time.Parse(time.RFC3339, state.SuccessFinished); e == nil {
			next = finished.Add(interval)
		}
	}
	if state.NextRunAt != "" {
		if retryAt, e := time.Parse(time.RFC3339, state.NextRunAt); e == nil && retryAt.After(next) {
			next = retryAt
		}
	}
	return next
}

// tick checks for due work once. A scan executes inside the tick: one scan
// at a time is the concurrency bound, and a slow scan naturally delays the
// next poll instead of stacking work.
func (s *Scheduler) tick(ctx context.Context) {
	if s.Scan == nil || s.Store == nil {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	cfg, err := LoadHostConfig(s.DataDir)
	if err != nil {
		s.log("warning", "host security schedule unavailable: %v", err)
		return
	}
	if !cfg.Reporting {
		return
	}
	interval, err := ValidateScanInterval(cfg.ScanInterval)
	if err != nil {
		s.log("warning", "host security schedule unavailable: %v", err)
		return
	}
	now := s.now()
	if now.Before(s.nextRun(ctx, interval)) {
		return
	}
	s.tryScan(ctx, interval, now)
}

// tryScan runs one scheduled scan under the host scan lock. A manual scan
// holding the lock skips this round; the manual scan's completion time
// re-anchors the cadence through the store.
func (s *Scheduler) tryScan(ctx context.Context, interval time.Duration, now time.Time) {
	lock, err := TryAcquireHostScanLock(s.DataDir)
	if errors.Is(err, ErrHostScanLockHeld) {
		s.log("info", "host security: a manual scan holds the scan lock; skipping this round")
		return
	}
	if err != nil {
		s.log("warning", "host security: could not take the scan lock: %v", err)
		return
	}
	defer lock.Release()

	state, err := loadHostSchedule(s.DataDir)
	if err != nil {
		s.log("warning", "host security: could not record the scan start: %v", err)
	}
	state.Running = true
	if err := saveHostSchedule(s.DataDir, state); err != nil {
		s.log("warning", "host security: could not record the scan start: %v", err)
	}

	scanErr := s.Scan(ctx)
	state, stateErr := loadHostSchedule(s.DataDir)
	if stateErr != nil {
		state = hostSchedule{}
	}
	state.Running = false
	if scanErr != nil {
		if ctx.Err() != nil {
			if saveErr := saveHostSchedule(s.DataDir, state); saveErr != nil {
				s.log("warning", "host security: could not persist schedule state: %v", saveErr)
			}
			return
		}
		backoff := s.nextBackoff(interval)
		state.Failures++
		state.NextRunAt = now.Add(backoff).Format(time.RFC3339)
		if saveErr := saveHostSchedule(s.DataDir, state); saveErr != nil {
			s.log("warning", "host security: could not schedule a retry: %v", saveErr)
		}
		s.log("warning", "host security: scheduled scan failed (%s); retrying in %s", scanErr, backoff)
		return
	}
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
	state.Failures = 0
	state.NextRunAt = ""
	finished := s.now()
	latest, latestErr := s.Store.LatestFinished(ctx, s.ServerID)
	if latestErr == nil && !latest.FinishedAt.IsZero() {
		finished = latest.FinishedAt
	}
	state.SuccessFinished = finished.Format(time.RFC3339)
	if saveErr := saveHostSchedule(s.DataDir, state); saveErr != nil {
		s.log("warning", "host security: could not persist schedule state: %v", saveErr)
	}
	s.log("info", "host security: scheduled scan completed; next run at %s", finished.Add(interval).Format(time.RFC3339))
}

// nextBackoff doubles per consecutive failure, capped by the interval.
func (s *Scheduler) nextBackoff(interval time.Duration) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	backoff := FailureBackoffBase
	for i := 1; i < s.failures && backoff < interval; i++ {
		backoff *= 2
	}
	if backoff > interval {
		backoff = interval
	}
	return backoff
}
