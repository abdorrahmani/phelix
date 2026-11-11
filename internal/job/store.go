// Package job runs one-shot commands on a cron schedule and persists their
// executions and output. It is the engine behind the `type: job` workload.
package job

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/processex"
)

const (
	schemaVersion = 1
	// Retention bounds how many finished executions are kept per app.
	Retention = 200
)

var idPattern = regexp.MustCompile(`^job_[0-9a-f]{16}$`)

// Status is the lifecycle of one job execution.
type Status string

const (
	StatusPending     Status = "pending"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusTimedOut    Status = "timed_out"
	StatusCancelled   Status = "cancelled"
	StatusSkipped     Status = "skipped"
	StatusInterrupted Status = "interrupted"
)

// Finished reports whether the execution will never change again.
func (s Status) Finished() bool {
	switch s {
	case StatusRunning, StatusPending:
		return false
	default:
		return true
	}
}

// Trigger records what started the execution.
type Trigger string

const (
	TriggerScheduled Trigger = "scheduled"
	TriggerManual    Trigger = "manual"
)

// Record is one persisted execution.
type Record struct {
	SchemaVersion int        `json:"schema_version"`
	ID            string     `json:"id"`
	AppName       string     `json:"app_name"`
	AppID         string     `json:"app_id,omitempty"`
	Trigger       Trigger    `json:"trigger"`
	Status        Status     `json:"status"`
	Command       []string   `json:"command"`
	Version       int        `json:"version,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	DurationMS    int64      `json:"duration_ms,omitempty"`
	ScheduledFor  *time.Time `json:"scheduled_for,omitempty"`
	Attempts      int        `json:"attempts,omitempty"`
	ExitCode      int        `json:"exit_code,omitempty"`
	PID           int        `json:"pid,omitempty"`
	ProcessStart  uint64     `json:"process_start_time,omitempty"`
	Executable    string     `json:"executable,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// NewID returns a fresh execution identifier.
func NewID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("job: cannot generate execution id: %v", err))
	}
	return "job_" + hex.EncodeToString(buf[:])
}

// Store persists executions and logs under <dataDir>/jobs/<app>.
type Store struct {
	root string
	// Now makes the clock injectable for tests.
	Now func() time.Time
	// Events receives lifecycle transitions when installed. Emission is
	// best-effort and never blocks or fails an execution.
	Events func(Event)
}

// NewStore returns the job store rooted at dataDir.
func NewStore(dataDir string) *Store {
	return &Store{root: filepath.Join(dataDir, "jobs"), Now: time.Now}
}

// Root reports the store root, for diagnostics.
func (s *Store) Root() string { return s.root }

func (s *Store) appDir(appName string) (string, error) {
	if err := deploy.ValidateAppName(appName); err != nil {
		return "", err
	}
	return filepath.Join(s.root, appName), nil
}

func (s *Store) executionsDir(appName string) (string, error) {
	dir, err := s.appDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "executions"), nil
}

func (s *Store) logsDir(appName string) (string, error) {
	dir, err := s.appDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "logs"), nil
}

func (s *Store) locksDir(appName string) (string, error) {
	dir, err := s.appDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "locks"), nil
}

func (s *Store) ensureDirs(appName string) error {
	for _, fn := range []func(string) (string, error){s.executionsDir, s.logsDir, s.locksDir} {
		dir, err := fn(appName)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: create %s", dir)
		}
	}
	return nil
}

// LogPath returns the absolute log path of one execution.
func (s *Store) LogPath(appName, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid execution id %q", id)
	}
	dir, err := s.logsDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".log"), nil
}

func (s *Store) recordPath(appName, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument, "invalid execution id %q", id)
	}
	dir, err := s.executionsDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// OpenLog opens the append-only log of one execution.
func (s *Store) OpenLog(appName, id string) (*os.File, error) {
	path, err := s.LogPath(appName, id)
	if err != nil {
		return nil, err
	}
	if err := s.ensureDirs(appName); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: open log %s", path)
	}
	return f, nil
}

// Save writes one record atomically.
func (s *Store) Save(record *Record) error {
	if record == nil || !idPattern.MatchString(record.ID) {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "cannot save a job record without a valid execution id")
	}
	if record.SchemaVersion == 0 {
		record.SchemaVersion = schemaVersion
	}
	if err := s.ensureDirs(record.AppName); err != nil {
		return err
	}
	path, err := s.recordPath(record.AppName, record.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "job: cannot encode execution record", err)
	}
	return writeFileAtomic(path, data, 0o600)
}

// Load reads one execution record. A record with a foreign schema version is
// rejected instead of guessed at.
func (s *Store) Load(appName, id string) (*Record, error) {
	path, err := s.recordPath(appName, id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, phelixerr.Newf(phelixerr.CodeNotFound, "no job execution %s for %q", id, appName)
	}
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: read execution %s", id)
	}
	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, phelixerr.Newf(phelixerr.CodeFilesystem, "job execution %s is corrupt: %v", id, err)
	}
	if record.ID != id || record.AppName != appName {
		return nil, phelixerr.Newf(phelixerr.CodeFilesystem, "job execution %s has an unexpected identity", id)
	}
	if record.SchemaVersion != schemaVersion {
		return nil, phelixerr.Newf(phelixerr.CodeFilesystem,
			"job execution %s has unsupported schema version %d", id, record.SchemaVersion)
	}
	return &record, nil
}

// List returns one app's executions newest first. Corrupt records are
// skipped so a single damaged file never hides the rest of the history.
func (s *Store) List(appName string, limit int) ([]*Record, error) {
	dir, err := s.executionsDir(appName)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: list executions for %q", appName)
	}
	var out []*Record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		record, err := s.Load(appName, id)
		if err != nil {
			continue
		}
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// HasScheduled reports whether an execution for the exact scheduled instant
// already exists. It is the last line of defense against running one cron
// occurrence twice across monitor restarts.
func (s *Store) HasScheduled(appName string, scheduledFor time.Time) bool {
	records, err := s.List(appName, 0)
	if err != nil {
		return false
	}
	for _, record := range records {
		if record.ScheduledFor != nil && record.ScheduledFor.Equal(scheduledFor) {
			return true
		}
	}
	return false
}

// Retain keeps the newest keep finished executions and removes the rest. A
// running or pending execution is never deleted, and its log stays with it.
func (s *Store) Retain(appName string, keep int) error {
	if keep < 0 {
		return nil
	}
	records, err := s.List(appName, 0)
	if err != nil {
		return err
	}
	for i, record := range records {
		if i < keep || !record.Status.Finished() {
			continue
		}
		path, err := s.recordPath(appName, record.ID)
		if err == nil {
			_ = os.Remove(path)
		}
		if logPath, err := s.LogPath(appName, record.ID); err == nil {
			_ = os.Remove(logPath)
		}
	}
	return nil
}

// RecoverInterrupted closes out running records left behind by a previous
// process. A record whose process still matches its captured identity is left
// alone: a manual run may outlive a monitor restart.
func (s *Store) RecoverInterrupted() (int, error) {
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: list apps")
	}
	recovered := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		appName := entry.Name()
		records, err := s.List(appName, 0)
		if err != nil {
			continue
		}
		for _, record := range records {
			if record.Status != StatusRunning && record.Status != StatusPending {
				continue
			}
			if record.PID > 0 && processex.Matches(processex.ProcessIdentity{
				PID:       record.PID,
				StartTime: record.ProcessStart,
				ExePath:   record.Executable,
			}) {
				continue
			}
			now := s.Now()
			record.Status = StatusInterrupted
			record.FinishedAt = &now
			record.DurationMS = now.Sub(record.StartedAt).Milliseconds()
			record.Error = "interrupted by a process restart"
			if err := s.Save(record); err != nil {
				return recovered, err
			}
			s.emitRecord(record, PhaseInterrupted, record.Attempts)
			recovered++
		}
	}
	return recovered, nil
}

// ErrRunLockHeld reports that another process is running this job.
var ErrRunLockHeld = errors.New("job run lock is held by another process")
