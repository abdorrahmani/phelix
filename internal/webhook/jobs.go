package webhook

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
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// Job lifecycle statuses. The running states (syncing…health_checking) are
// ordered; transitions must move forward. Terminal states are final.
const (
	StatusAccepted       = "accepted"
	StatusQueued         = "queued"
	StatusSyncing        = "syncing"
	StatusBuilding       = "building"
	StatusDeploying      = "deploying"
	StatusHealthChecking = "health_checking"
	StatusSucceeded      = "succeeded"
	StatusFailed         = "failed"
	StatusRolledBack     = "rolled_back"
	StatusCancelled      = "cancelled"
)

// Job stages track where a job is independently of its status, including
// phases that are not statuses (queue wait, cleanup, restart recovery).
const (
	StageQueue       = "queue"
	StageGitSync     = "git_sync"
	StageBuild       = "build"
	StageDeploy      = "deploy"
	StageHealthCheck = "health_check"
	StageCleanup     = "cleanup"
	StageRecovery    = "recovery"
)

// Job error codes (phelixerr-style strings; see classifyRebuildFailure for
// how the rebuild exit code maps onto them).
const (
	JobErrGitSync     = "GIT_SYNC_FAILED"
	JobErrBuild       = "BUILD_FAILED"
	JobErrDeploy      = "DEPLOY_FAILED"
	JobErrHealth      = "HEALTH_CHECK_FAILED"
	JobErrAutoRoll    = "AUTO_ROLLBACK_FAILED"
	JobErrRollback    = "ROLLBACK_FAILED"
	JobErrRebuild     = "REBUILD_FAILED"
	JobErrQueue       = "WEBHOOK_QUEUE_FAILED"
	JobErrInterrupted = "WEBHOOK_JOB_INTERRUPTED"
)

// DefaultJobRetention bounds the job history (most recent jobs kept; the
// delivery ledger's dedup window is separate and unaffected).
const DefaultJobRetention = 200

// jobSchemaVersion guards the per-job record files against format drift.
const jobSchemaVersion = 1

// statusRank orders the running states; terminal states are -1.
var statusRank = map[string]int{
	StatusAccepted:       0,
	StatusQueued:         1,
	StatusSyncing:        2,
	StatusBuilding:       3,
	StatusDeploying:      4,
	StatusHealthChecking: 5,
}

func isTerminalStatus(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusRolledBack, StatusCancelled:
		return true
	}
	return false
}

// JobRecord is the durable deployment job: one accepted webhook delivery and
// everything Phelix knows about what happened to it. It never stores the HMAC
// secret, request bodies, or temporary worktree paths.
type JobRecord struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"` // wh_… — the Phelix deployment job id
	DeliveryID    string `json:"delivery_id"`
	AppID         string `json:"app_id"`
	AppName       string `json:"app_name"`
	Branch        string `json:"branch"`
	Commit        string `json:"commit"`
	Provider      string `json:"provider"`
	Status        string `json:"status"`
	Stage         string `json:"stage"`

	AcceptedAt int64 `json:"accepted_at"` // unix millis
	StartedAt  int64 `json:"started_at,omitempty"`
	FinishedAt int64 `json:"finished_at,omitempty"`

	Version      int    `json:"version,omitempty"` // Phelix version the job deployed (or restored, for rolled_back)
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// Terminal reports whether the job reached a final state.
func (r *JobRecord) Terminal() bool { return isTerminalStatus(r.Status) }

// Active reports whether the job is still in flight.
func (r *JobRecord) Active() bool { return !isTerminalStatus(r.Status) && r.Status != "" }

var jobIDPattern = regexp.MustCompile(`^wh_[0-9a-f]{16}$`)

// NewJobID generates a fresh deployment job id (wh_ + 16 hex chars).
func NewJobID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is practically unreachable; fall back to a
		// time-derived id so job creation never blocks on entropy.
		return fmt.Sprintf("wh_%016x", time.Now().UnixNano())
	}
	return "wh_" + hex.EncodeToString(buf)
}

// JobStore is the durable webhook job history: one JSON file per job under
// dir, written atomically (tmp + fsync + rename). Per-job files mean one
// job's update can never corrupt or overwrite another's, and a partial write
// damages exactly one record — which the next load moves aside and skips.
// This store is independent from the delivery ledger: the ledger is the
// replay-protection window, this is queryable history, and their retention
// policies are separate.
type JobStore struct {
	mu    sync.Mutex
	dir   string
	max   int
	jobs  map[string]*JobRecord
	ready bool
	err   error
	// readOnly suppresses repairs (corrupt files are skipped, not renamed)
	// and retention sweeps — the CLI must never mutate daemon state.
	readOnly bool
}

// NewJobStore creates a writable store (the daemon).
func NewJobStore(dir string, max int) *JobStore {
	return &JobStore{dir: dir, max: max, jobs: make(map[string]*JobRecord)}
}

// OpenJobStoreReadOnly creates a read-only store (the status/history CLI
// commands): loads never repair or sweep, only read.
func OpenJobStoreReadOnly(dir string) *JobStore {
	return &JobStore{dir: dir, max: 0, jobs: make(map[string]*JobRecord), readOnly: true}
}

// Load reads all job records from disk. A missing directory is an empty
// store. Individual malformed/corrupt files are skipped (and, in writable
// mode, moved aside as *.corrupt.<unix> like the app-state recovery) so one
// damaged record cannot take the whole history down.
func (s *JobStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return s.err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil && !s.readOnly {
		s.err = phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "webhook jobs: create %s", s.dir)
		s.ready = true
		return s.err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			s.ready = true
			return nil
		}
		if !s.readOnly {
			s.err = phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "webhook jobs: read %s", s.dir)
			s.ready = true
			return s.err
		}
		// Read-only mode tolerates an unreadable directory as "no jobs" so
		// the CLI stays useful on machines where the daemon never ran.
		s.ready = true
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		rec, err := loadJobRecord(path)
		if err != nil {
			if s.readOnly {
				logs.WarningFile("webhook", "job store: skipping unreadable record %s: %v", e.Name(), err)
				continue
			}
			// Move the damaged record aside; the rest of the history stays.
			_ = os.Rename(path, fmt.Sprintf("%s.corrupt.%d", path, time.Now().Unix()))
			logs.Warning("webhook", "job store: moved corrupt record aside: %s (%v)", e.Name(), err)
			continue
		}
		s.jobs[rec.ID] = rec
	}
	s.ready = true
	return nil
}

func loadJobRecord(path string) (*JobRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec JobRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.SchemaVersion != jobSchemaVersion {
		return nil, fmt.Errorf("unsupported schema version %d", rec.SchemaVersion)
	}
	if !jobIDPattern.MatchString(rec.ID) || rec.AppName == "" || rec.DeliveryID == "" ||
		rec.Status == "" || rec.AcceptedAt == 0 {
		return nil, errors.New("invalid job record")
	}
	return &rec, nil
}

// Create persists a new job record. This is the durability gate for webhook
// acceptance: the handler must not acknowledge a delivery until Create
// succeeded, so a crash can never lose an accepted job.
func (s *JobStore) Create(rec *JobRecord) error {
	if rec == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook jobs: nil record")
	}
	if rec.ID == "" {
		rec.ID = NewJobID()
	}
	if !jobIDPattern.MatchString(rec.ID) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "webhook jobs: invalid id %q", rec.ID)
	}
	if rec.Status == "" {
		rec.Status = StatusAccepted
	}
	if rec.Stage == "" {
		rec.Stage = StageQueue
	}
	if rec.AcceptedAt == 0 {
		rec.AcceptedAt = time.Now().UnixMilli()
	}
	rec.SchemaVersion = jobSchemaVersion

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(); err != nil {
		return err
	}
	if _, exists := s.jobs[rec.ID]; exists {
		return phelixerr.Newf(phelixerr.CodeAlreadyExists, "webhook jobs: %s already exists", rec.ID)
	}
	if err := s.persistLocked(rec); err != nil {
		return err
	}
	s.jobs[rec.ID] = rec
	s.sweepRetentionLocked()
	return nil
}

// update applies fn to the job's record after transition validation and
// persists the result. fn must not change the ID.
func (s *JobStore) update(id string, fn func(*JobRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(); err != nil {
		return err
	}
	rec, ok := s.jobs[id]
	if !ok {
		return phelixerr.Newf(phelixerr.CodeNotFound, "webhook jobs: unknown job %s", id)
	}
	before := *rec
	if err := fn(rec); err != nil {
		*rec = before // roll back the in-memory mutation
		return err
	}
	if rec.ID != before.ID {
		*rec = before
		return phelixerr.New(phelixerr.CodeInvalidArgument, "webhook jobs: id is immutable")
	}
	if err := s.persistLocked(rec); err != nil {
		*rec = before // keep memory consistent with what is on disk
		return err
	}
	return nil
}

// forwardTransition validates a move to a new running status (or stage-only
// refresh): running statuses must never move backward.
func forwardTransition(rec *JobRecord, newStatus string) error {
	if isTerminalStatus(rec.Status) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"webhook jobs: %s is terminal (%s); no further transitions", rec.ID, rec.Status)
	}
	oldRank, oldOK := statusRank[rec.Status]
	newRank, newOK := statusRank[newStatus]
	if !oldOK || !newOK {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"webhook jobs: %s: %q is not a running status", rec.ID, newStatus)
	}
	if newRank < oldRank {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"webhook jobs: %s: cannot move backward from %s to %s", rec.ID, rec.Status, newStatus)
	}
	return nil
}

// terminalTransition validates a move from any running state to a terminal
// state (with the finished timestamp stamped by the caller helpers).
func terminalTransition(rec *JobRecord, status string) error {
	if isTerminalStatus(rec.Status) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"webhook jobs: %s is already terminal (%s)", rec.ID, rec.Status)
	}
	if !isTerminalStatus(status) {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"webhook jobs: %s: %q is not a terminal status", rec.ID, status)
	}
	return nil
}

// MarkQueued records that the job was handed to the build queue.
func (s *JobStore) MarkQueued(id string) error {
	return s.update(id, func(rec *JobRecord) error {
		if err := forwardTransition(rec, StatusQueued); err != nil {
			return err
		}
		rec.Status = StatusQueued
		rec.Stage = StageQueue
		return nil
	})
}

// MarkSyncing records that the worker picked the job up and Git
// synchronization is starting.
func (s *JobStore) MarkSyncing(id string) error {
	return s.update(id, func(rec *JobRecord) error {
		if err := forwardTransition(rec, StatusSyncing); err != nil {
			return err
		}
		rec.Status = StatusSyncing
		rec.Stage = StageGitSync
		if rec.StartedAt == 0 {
			rec.StartedAt = time.Now().UnixMilli()
		}
		return nil
	})
}

// MarkBuilding records that the rebuild command has been spawned.
func (s *JobStore) MarkBuilding(id string) error {
	return s.update(id, func(rec *JobRecord) error {
		if err := forwardTransition(rec, StatusBuilding); err != nil {
			return err
		}
		rec.Status = StatusBuilding
		rec.Stage = StageBuild
		return nil
	})
}

// MarkProgress advances the running status/stage from the rebuild pipeline's
// own progress output (deploy / health check). Forward-only, best-effort.
func (s *JobStore) MarkProgress(id, status, stage string) error {
	return s.update(id, func(rec *JobRecord) error {
		if err := forwardTransition(rec, status); err != nil {
			return err
		}
		rec.Status = status
		rec.Stage = stage
		return nil
	})
}

// MarkCleanup records that the rebuild has finished (any outcome) and the
// isolated source is being removed. Status is unchanged.
func (s *JobStore) MarkCleanup(id string) error {
	return s.update(id, func(rec *JobRecord) error {
		if isTerminalStatus(rec.Status) {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"webhook jobs: %s is terminal; cleanup stage is meaningless", rec.ID)
		}
		rec.Stage = StageCleanup
		return nil
	})
}

// MarkSucceeded records a completed deployment, correlated with the Phelix
// version it produced (version may be 0 when the pipeline promoted nothing).
func (s *JobStore) MarkSucceeded(id string, version int) error {
	return s.update(id, func(rec *JobRecord) error {
		if err := terminalTransition(rec, StatusSucceeded); err != nil {
			return err
		}
		rec.Status = StatusSucceeded
		rec.Stage = StageCleanup
		rec.Version = version
		rec.ErrorCode = ""
		rec.ErrorMessage = ""
		rec.FinishedAt = time.Now().UnixMilli()
		return nil
	})
}

// MarkFailed records a failed job with a classified error code and a
// sanitized message.
func (s *JobStore) MarkFailed(id, errorCode, message string) error {
	return s.update(id, func(rec *JobRecord) error {
		if err := terminalTransition(rec, StatusFailed); err != nil {
			return err
		}
		rec.Status = StatusFailed
		rec.Stage = StageCleanup
		rec.ErrorCode = errorCode
		rec.ErrorMessage = SanitizeJobMessage(message)
		rec.FinishedAt = time.Now().UnixMilli()
		return nil
	})
}

// MarkRolledBack records a deployment that failed and was automatically
// restored to a previous version (version = the restored one, when known).
func (s *JobStore) MarkRolledBack(id string, version int, errorCode, message string) error {
	return s.update(id, func(rec *JobRecord) error {
		if err := terminalTransition(rec, StatusRolledBack); err != nil {
			return err
		}
		rec.Status = StatusRolledBack
		rec.Stage = StageCleanup
		rec.Version = version
		rec.ErrorCode = errorCode
		rec.ErrorMessage = SanitizeJobMessage(message)
		rec.FinishedAt = time.Now().UnixMilli()
		return nil
	})
}

// MarkCancelled records a job dropped before it started (daemon shutdown
// while queued). A job whose rebuild subprocess is still running is NOT
// cancelled — it stays non-terminal so restart recovery can resolve it.
func (s *JobStore) MarkCancelled(id, message string) error {
	return s.update(id, func(rec *JobRecord) error {
		if err := terminalTransition(rec, StatusCancelled); err != nil {
			return err
		}
		rec.Status = StatusCancelled
		rec.Stage = StageQueue
		rec.ErrorCode = JobErrQueue
		rec.ErrorMessage = SanitizeJobMessage(message)
		rec.FinishedAt = time.Now().UnixMilli()
		return nil
	})
}

// Get returns a copy of the job record, if present.
func (s *JobStore) Get(id string) (JobRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.jobs[id]
	if !ok {
		return JobRecord{}, false
	}
	return *rec, true
}

// ActiveForApp returns the app's in-flight jobs, newest first.
func (s *JobStore) ActiveForApp(app string) []JobRecord {
	return s.filterForApp(app, func(r *JobRecord) bool { return r.Active() })
}

// HistoryForApp returns the app's terminal jobs, newest first, up to limit.
func (s *JobStore) HistoryForApp(app string, limit int) []JobRecord {
	return s.filterForApp(app, func(r *JobRecord) bool { return r.Terminal() }, limit)
}

func (s *JobStore) filterForApp(app string, keep func(*JobRecord) bool, limit ...int) []JobRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []JobRecord
	for _, rec := range s.jobs {
		if (rec.AppName == app || rec.AppID == app) && keep(rec) {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AcceptedAt != out[j].AcceptedAt {
			return out[i].AcceptedAt > out[j].AcceptedAt
		}
		return out[i].ID > out[j].ID
	})
	if len(limit) > 0 && limit[0] > 0 && len(out) > limit[0] {
		out = out[:limit[0]]
	}
	return out
}

// Count reports the number of stored jobs (diagnostics and tests).
func (s *JobStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// writableLocked is the shared precondition for mutations.
func (s *JobStore) writableLocked() error {
	if s.readOnly {
		return phelixerr.New(phelixerr.CodePermissionDenied, "webhook jobs: store is read-only")
	}
	if s.err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "webhook jobs unavailable", s.err)
	}
	return nil
}

// sweepRetentionLocked bounds the history: drop the oldest TERMINAL records
// above capacity. Active jobs are never deleted — an unbounded active set is
// impossible anyway (per-app queue depth is bounded).
func (s *JobStore) sweepRetentionLocked() {
	if s.max <= 0 {
		return
	}
	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.jobs[ids[i]], s.jobs[ids[j]]
		if a.AcceptedAt != b.AcceptedAt {
			return a.AcceptedAt < b.AcceptedAt
		}
		return a.ID < b.ID
	})
	excess := len(ids) - s.max
	for _, id := range ids {
		if excess <= 0 {
			break
		}
		if !s.jobs[id].Terminal() {
			continue
		}
		if err := os.Remove(s.jobPath(id)); err != nil && !os.IsNotExist(err) {
			logs.Warning("webhook", "job store: could not remove expired record %s: %v", id, err)
			continue
		}
		delete(s.jobs, id)
		excess--
	}
}

func (s *JobStore) jobPath(id string) string { return filepath.Join(s.dir, id+".json") }

// persistLocked atomically writes one job record (tmp + fsync + rename).
func (s *JobStore) persistLocked(rec *JobRecord) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "webhook jobs: create %s", s.dir)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "webhook jobs: encode record", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%s", s.jobPath(rec.ID), os.Getpid(), "job")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "webhook jobs: create temp record", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "webhook jobs: write record", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "webhook jobs: sync record", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "webhook jobs: close record", err)
	}
	if err := os.Rename(tmp, s.jobPath(rec.ID)); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "webhook jobs: replace record", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Restart recovery
// ---------------------------------------------------------------------------

// RecoveryOutcome describes what restart recovery decided about one
// interrupted job.
type RecoveryOutcome struct {
	JobID    string
	Previous string // status before recovery
	Result   string // StatusSucceeded or StatusFailed
	Version  int
}

// RecoverInterrupted resolves jobs left non-terminal by a daemon crash or
// shutdown. Nothing is ever re-run: the delivery ledger keeps the replay
// guarantee, and this pass only closes the records.
//
// For jobs that had already spawned their rebuild (building/deploying/
// health_checking) the existing deployment state decides the outcome: when
// the app's CURRENT version is the job's exact pushed commit and was
// promoted after the job was accepted, the orphaned rebuild demonstrably
// finished — the job is marked succeeded with that version. In every other
// case the honest answer is an explicit interruption failure
// (WEBHOOK_JOB_INTERRUPTED), never an invented success.
func (s *JobStore) RecoverInterrupted(currentVersion func(appName string) (*deploy.VersionMeta, error)) []RecoveryOutcome {
	if currentVersion == nil {
		currentVersion = func(string) (*deploy.VersionMeta, error) { return nil, nil }
	}

	s.mu.Lock()
	var interrupted []*JobRecord
	for _, rec := range s.jobs {
		if rec.Active() {
			interrupted = append(interrupted, rec)
		}
	}
	s.mu.Unlock()

	var outcomes []RecoveryOutcome
	for _, rec := range interrupted {
		// Operate on a copy; each Mark* re-locks the store.
		job := *rec
		var outcome RecoveryOutcome
		outcome.JobID = job.ID
		outcome.Previous = job.Status

		resolved := false
		if statusRank[job.Status] >= statusRank[StatusBuilding] {
			if meta, err := currentVersion(job.AppName); err == nil && meta != nil &&
				strings.EqualFold(meta.GitCommit, job.Commit) &&
				meta.DeployedAt != nil && meta.DeployedAt.UnixMilli() >= job.AcceptedAt {
				if err := s.MarkSucceeded(job.ID, meta.Version); err == nil {
					outcome.Result = StatusSucceeded
					outcome.Version = meta.Version
					resolved = true
				}
			}
		}
		if !resolved {
			msg := fmt.Sprintf("daemon restart interrupted the job while %s; the deployment was not re-run (redeliver the webhook to deploy again)", job.Status)
			if err := s.update(job.ID, func(rec *JobRecord) error {
				if err := terminalTransition(rec, StatusFailed); err != nil {
					return err
				}
				rec.Status = StatusFailed
				rec.Stage = StageRecovery
				rec.ErrorCode = JobErrInterrupted
				rec.ErrorMessage = SanitizeJobMessage(msg)
				rec.FinishedAt = time.Now().UnixMilli()
				return nil
			}); err == nil {
				outcome.Result = StatusFailed
			}
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// SanitizeJobMessage makes an error message safe to persist in a job record:
// redacted (the shared redactor strips credentials/tokens) and bounded.
func SanitizeJobMessage(msg string) string {
	msg = phelixerr.Redact(msg)
	msg = strings.TrimSpace(msg)
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}
