package job

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// SchedulerPollInterval is how often due jobs are checked.
const SchedulerPollInterval = time.Second

// ScheduledJob is one job as the scheduler sees it, already resolved from
// phelix.yaml.
type ScheduledJob struct {
	AppName     string
	AppID       string
	Dir         string
	Cron        string
	Location    *time.Location
	Command     []string
	Timeout     time.Duration
	Retries     int
	Concurrency string
	Version     int
}

type jobState struct {
	cron     string
	location string
	schedule cron.Schedule
	next     time.Time
}

// Scheduler runs due jobs inside the monitor daemon. It is the only
// scheduler: the CLI runs jobs only on explicit request.
type Scheduler struct {
	Executor *Executor
	// ListJobs returns every configured job. It is called once per tick.
	ListJobs func() []ScheduledJob
	// PollInterval is how often due work is checked; zero uses the default.
	PollInterval time.Duration
	// Now makes the clock injectable for tests.
	Now func() time.Time
	// Log receives diagnostics, never command output.
	Log func(level string, format string, args ...any)

	mu     sync.Mutex
	states map[string]*jobState
}

// NewScheduler builds a scheduler over the given executor.
func NewScheduler(executor *Executor) *Scheduler {
	return &Scheduler{
		Executor: executor,
		Now:      time.Now,
		states:   map[string]*jobState{},
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
// executions abandoned by a previous process interrupted, then checks for due
// work. Missed occurrences are skipped, never replayed in a burst.
func (s *Scheduler) Run(ctx context.Context) {
	if s.Executor != nil && s.Executor.Store != nil {
		if recovered, err := s.Executor.Store.RecoverInterrupted(); err != nil {
			s.log("warning", "job: could not recover interrupted executions: %v", err)
		} else if recovered > 0 {
			s.log("info", "job: marked %d interrupted execution(s) from a previous process", recovered)
		}
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = SchedulerPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			s.log("info", "job scheduler stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick checks every job once. An execution runs to completion before the next
// job is considered, so a slow run delays later work instead of stacking it.
func (s *Scheduler) tick(ctx context.Context) {
	if s.ListJobs == nil || s.Executor == nil {
		return
	}
	now := s.now()
	seen := make(map[string]bool)
	for _, job := range s.ListJobs() {
		if ctx.Err() != nil {
			return
		}
		if job.AppName == "" || strings.TrimSpace(job.Cron) == "" || job.Dir == "" {
			continue
		}
		seen[job.AppName] = true
		s.runDue(ctx, job, now)
	}
	s.mu.Lock()
	for key := range s.states {
		if !seen[key] {
			delete(s.states, key)
		}
	}
	s.mu.Unlock()
}

// runDue starts the job when its next occurrence has arrived, then advances
// past every occurrence that elapsed while it ran.
func (s *Scheduler) runDue(ctx context.Context, job ScheduledJob, now time.Time) {
	state, err := s.stateFor(job, now)
	if err != nil {
		s.log("warning", "job: %s: cannot parse schedule %q: %v", job.AppName, job.Cron, err)
		return
	}
	if state.next.IsZero() || state.next.After(now) {
		return
	}
	scheduled := state.next
	if s.Executor.Store.HasScheduled(job.AppName, scheduled) {
		s.advance(state, job, s.now())
		return
	}

	req := RunRequest{
		AppName:      job.AppName,
		AppID:        job.AppID,
		Dir:          job.Dir,
		Command:      job.Command,
		Timeout:      job.Timeout,
		Retries:      job.Retries,
		Concurrency:  job.Concurrency,
		Trigger:      TriggerScheduled,
		ScheduledFor: &scheduled,
		Version:      job.Version,
	}
	record, err := s.Executor.Run(ctx, req)
	if errors.Is(err, ErrRunLockHeld) {
		if _, skipErr := s.Executor.Skip(req, scheduled, "another run of this job is in progress"); skipErr != nil {
			s.log("warning", "job: %s: could not record the skipped occurrence: %v", job.AppName, skipErr)
		}
		s.log("info", "job: %s: previous run still active; skipped the %s occurrence", job.AppName, scheduled.Format(time.RFC3339))
	} else if err != nil {
		if ctx.Err() == nil {
			s.log("warning", "job: %s: could not run the scheduled occurrence: %v", job.AppName, err)
		}
	} else if record != nil {
		s.log("info", "job: %s: %s (%s)", job.AppName, record.Status, record.ID)
	}
	s.advance(state, job, s.now())
}

// stateFor parses and caches one job's schedule. A changed expression or
// timezone rebuilds the state from the current time.
func (s *Scheduler) stateFor(job ScheduledJob, now time.Time) (*jobState, error) {
	location := job.Location
	if location == nil {
		location = time.UTC
	}
	spec, err := cron.ParseStandard(strings.TrimSpace(job.Cron))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.states[job.AppName]; state != nil && state.cron == job.Cron && state.location == location.String() {
		return state, nil
	}
	state := &jobState{
		cron:     job.Cron,
		location: location.String(),
		schedule: spec,
		next:     spec.Next(now.In(location)),
	}
	s.states[job.AppName] = state
	return state, nil
}

// advance moves the schedule past the current instant. Intermediate
// occurrences are skipped on purpose: a job that ran longer than its interval
// never causes a catch-up burst.
func (s *Scheduler) advance(state *jobState, job ScheduledJob, now time.Time) {
	location := job.Location
	if location == nil {
		location = time.UTC
	}
	state.next = state.schedule.Next(now.In(location))
}
