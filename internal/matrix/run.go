package matrix

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// RunID identifies one matrix execution. Format: mx_YYYYMMDD_xxxx where xxxx
// is 4 random hex characters — unique per day in practice (the history store
// rejects collisions), stable for the whole run, safe for filenames and CLI
// usage, and readable by humans.
type RunID string

// runIDPattern is the single authority on what a well-formed RunID looks
// like. LoadRun uses it to reject arbitrary user input before it ever reaches
// the filesystem (path traversal, empty IDs, …).
var runIDPattern = regexp.MustCompile(`^mx_\d{8}_[0-9a-f]{4}$`)

// runIDEntropy is the randomness source for RunID suffixes. It is a seam so
// tests can force collisions deterministically; production uses crypto/rand.
var runIDEntropy = rand.Read

// NewRunID mints a RunID for the given time. Randomness comes from crypto/rand;
// an entropy failure falls back to a timestamp-derived suffix, which still
// yields a valid (if less random) ID rather than failing the build. Collisions
// are guarded downstream by NewUniqueRunID and SaveRun.
func NewRunID(now time.Time) RunID {
	suffix := make([]byte, 2)
	if _, err := runIDEntropy(suffix); err != nil {
		// Fall back to sub-second entropy: still unique within the process,
		// still matching the pattern.
		suffix[0] = byte(now.Nanosecond() >> 8)
		suffix[1] = byte(now.Nanosecond())
	}
	return RunID(fmt.Sprintf("mx_%s_%s", now.Format("20060102"), hex.EncodeToString(suffix)))
}

// ParseRunID validates the user-supplied string in `phelix matrix show <id>`
// before any filesystem access.
func ParseRunID(s string) (RunID, error) {
	if !runIDPattern.MatchString(s) {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid matrix run ID %q — expected format mx_YYYYMMDD_xxxx (e.g. mx_20260909_8f31); see 'phelix matrix list'", s)
	}
	return RunID(s), nil
}

// NewUniqueRunID mints a RunID that does not collide with an already
// persisted run, so the 4-hex random suffix stays collision-free in practice.
// If the history cannot be read it degrades to a plain NewRunID — SaveRun
// still rejects duplicates as a last resort.
func NewUniqueRunID(now time.Time) RunID {
	for i := 0; i < 8; i++ {
		id := NewRunID(now)
		_, err := LoadRun(id)
		if phelixerr.CodeOf(err) == phelixerr.CodeNotFound {
			return id
		}
	}
	return NewRunID(now)
}

// Run statuses. "running" is only ever persisted by a live execution; a
// persisted "running" record whose process is gone (crash, SIGKILL, machine
// restart) is treated as orphaned and is resumable. "interrupted" is the
// terminal-ish status of a run that stopped with incomplete combinations
// (SIGINT/SIGTERM handled, or a resume that was itself interrupted).
type RunStatus string

const (
	RunStatusRunning     RunStatus = "running"
	RunStatusSucceeded   RunStatus = "succeeded"
	RunStatusPartial     RunStatus = "partial"
	RunStatusFailed      RunStatus = "failed"
	RunStatusInterrupted RunStatus = "interrupted"
)

// IsTerminalRunStatus reports whether a run status is final: terminal runs are
// immutable in the history store (UpdateRun refuses to rewrite them), which is
// what keeps the original run unchanged after a manual retry.
func IsTerminalRunStatus(s RunStatus) bool {
	switch s {
	case RunStatusSucceeded, RunStatusPartial, RunStatusFailed:
		return true
	}
	return false
}

// RunConfig is the snapshot of the effective matrix configuration a run
// executed with. It is captured at run start and never re-read from
// phelix.yaml, so later configuration edits cannot rewrite what an old run
// claims to have built. Resume and manual retry reconstruct their execution
// from this snapshot, not from the current profile.
type RunConfig struct {
	Lang        string   `json:"language"`
	Versions    []string `json:"versions"`
	Platforms   []string `json:"platforms"`
	Concurrency int      `json:"concurrency"`
	// Include/Exclude are the effective rules the run's combinations were
	// expanded with (the full dimension model, not the raw YAML).
	Include []Rule `json:"include,omitempty"`
	Exclude []Rule `json:"exclude,omitempty"`
	// Retries is the automatic-retry budget the run executed with.
	Retries int `json:"retries,omitempty"`
	// BuildArgs are the extra build arguments the run executed with, so a
	// resume or manual retry reproduces the original build invocation.
	BuildArgs []string `json:"build_args,omitempty"`
	// Sources records where each dimension came from ("cli", "phelix.yaml",
	// "default", "detected") at execution time.
	Sources RunConfigSources `json:"sources"`
}

// RunConfigSources is the provenance part of a RunConfig snapshot.
type RunConfigSources struct {
	Lang        string `json:"language"`
	Versions    string `json:"versions"`
	Platforms   string `json:"platforms"`
	Concurrency string `json:"concurrency"`
	Retries     string `json:"retries,omitempty"`
	Include     string `json:"include,omitempty"`
	Exclude     string `json:"exclude,omitempty"`
}

// Profile reconstructs a normalized Profile from a persisted RunConfig
// snapshot. Resume and manual retry use it so they operate on the original
// run's effective configuration even after phelix.yaml changed.
func (rc RunConfig) Profile() *Profile {
	prof := &Profile{
		Lang:        builder.ParseLanguage(rc.Lang),
		Versions:    append([]string(nil), rc.Versions...),
		Platforms:   append([]string(nil), rc.Platforms...),
		Concurrency: rc.Concurrency,
		Include:     append([]Rule(nil), rc.Include...),
		Exclude:     append([]Rule(nil), rc.Exclude...),
		Retries:     rc.Retries,
	}
	if prof.Concurrency <= 0 {
		prof.Concurrency = DefaultConcurrency
	}
	return prof
}

// RunAttempt is one recorded execution attempt of one combination. Attempt 1
// is the original execution; automatic retries append 2, 3, … within the same
// run and combination.
type RunAttempt struct {
	Number   int    `json:"number"`
	Status   string `json:"status"`
	Duration string `json:"duration"`
	Error    string `json:"error,omitempty"`
}

// RunCombination is one combination's outcome inside a Run, carrying enough
// identity (run ID + all dimensions) to relate it unambiguously to its
// artifact and build result. Status is one of "pending", "running",
// "success", "failed", "skipped"; pending/running entries mean the
// combination has not completed (yet) and make the run resumable.
type RunCombination struct {
	ID        string `json:"id"`       // combination ID, e.g. "go1.26-linux-amd64"
	Identity  string `json:"identity"` // run-scoped identity: "<runID>/<combinationID>"
	Toolchain string `json:"toolchain"`
	Version   string `json:"toolchain_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Variant   string `json:"variant,omitempty"`
	Platform  string `json:"platform"`
	Status    string `json:"status"`
	Duration  string `json:"duration"`
	Artifact  string `json:"artifact,omitempty"`
	// SHA256 is the checksum of the final artifact's bytes (hex), or the
	// image digest for Docker image artifacts. Empty for failed/incomplete
	// combinations — only a successful, verified artifact carries one.
	SHA256 string `json:"sha256,omitempty"`
	// StartedAt records when this combination's current attempt began
	// executing (set while running; kept as history once completed).
	StartedAt   time.Time         `json:"started_at,omitempty"`
	Error       string            `json:"error,omitempty"`
	CacheStatus string            `json:"cache_status,omitempty"`
	Attempts    int               `json:"attempts,omitempty"` // attempt count; 1 when never retried
	AttemptLog  []RunAttempt      `json:"attempt_log,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"` // include-rule attributes
}

// Combination reconstructs the executable Combination from a persisted
// RunCombination entry.
func (rc RunCombination) Combination() Combination {
	return Combination{
		Lang:     builder.ParseLanguage(rc.Toolchain),
		Version:  rc.Version,
		OS:       rc.OS,
		Arch:     rc.Arch,
		Variant:  rc.Variant,
		Platform: rc.Platform,
		Metadata: rc.Metadata,
	}
}

// Run is the logical wrapper around one matrix execution: it aggregates the
// run ID, the configuration snapshot, timing, per-combination results, and
// the overall status. It is the entity the local history persists and the
// future backend/gRPC phase will adopt as-is.
//
// A manual retry never mutates its source Run: it creates a new Run with
// ParentRunID pointing back at the original.
type Run struct {
	ID         RunID     `json:"id"`
	AppName    string    `json:"app_name"`
	ProjectDir string    `json:"project_dir,omitempty"`
	Status     RunStatus `json:"status"`
	Config     RunConfig `json:"config"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Duration   string    `json:"duration,omitempty"`
	Total      int       `json:"total"`
	Succeeded  int       `json:"succeeded"`
	Failed     int       `json:"failed"`
	Skipped    int       `json:"skipped"`
	// Incomplete counts pending/running combinations (only non-zero for
	// interrupted runs).
	Incomplete   int              `json:"incomplete,omitempty"`
	Combinations []RunCombination `json:"combinations"`
	// ParentRunID links a manual retry run to the run it was derived from.
	// Empty for ordinary runs.
	ParentRunID RunID `json:"parent_run_id,omitempty"`
	// ResumeCount records how many times this run was resumed (0 = never).
	ResumeCount int `json:"resume_count,omitempty"`

	// mu guards the counters and combination entries against concurrent
	// RecordResult calls from executor workers. Unexported: never serialized.
	mu sync.Mutex `json:"-"`
}

// NewRun creates a running Run from a resolved profile. The configuration
// snapshot is taken here, at execution time.
func NewRun(id RunID, appName, projectDir string, prof *Profile, started time.Time) *Run {
	r := &Run{
		ID:         id,
		AppName:    appName,
		ProjectDir: projectDir,
		Status:     RunStatusRunning,
		StartedAt:  started,
	}
	if prof != nil {
		r.Config = RunConfig{
			Lang:        string(prof.Lang),
			Versions:    append([]string(nil), prof.Versions...),
			Platforms:   append([]string(nil), prof.Platforms...),
			Concurrency: prof.Concurrency,
			Include:     append([]Rule(nil), prof.Include...),
			Exclude:     append([]Rule(nil), prof.Exclude...),
			Retries:     prof.Retries,
			Sources: RunConfigSources{
				Lang:        prof.Source.Lang,
				Versions:    prof.Source.Versions,
				Platforms:   prof.Source.Platforms,
				Concurrency: prof.Source.Concurrency,
				Retries:     prof.Source.Retries,
				Include:     prof.Source.Include,
				Exclude:     prof.Source.Exclude,
			},
		}
	}
	return r
}

// NewRetryRun creates a new run derived from a source run for a manual retry
// (`phelix matrix retry <run-id>`). The configuration snapshot is copied from
// the source (the retry executes the original effective configuration, not
// the current phelix.yaml) and the new run records its parent. The source run
// is never modified.
func NewRetryRun(id RunID, source *Run, started time.Time) *Run {
	if source == nil {
		return nil
	}
	r := NewRun(id, source.AppName, source.ProjectDir, nil, started)
	cfg := source.Config
	cfg.Include = append([]Rule(nil), source.Config.Include...)
	cfg.Exclude = append([]Rule(nil), source.Config.Exclude...)
	r.Config = cfg
	r.ParentRunID = source.ID
	return r
}

// InitCombinations seeds the run with pending entries for every combination
// of the plan, in plan order. Callers then RecordResult each completed
// combination. It is idempotent per combination ID: combinations already
// present keep their recorded outcome (resume).
func (r *Run) InitCombinations(combs []Combination) {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing := make(map[string]bool, len(r.Combinations))
	for _, rc := range r.Combinations {
		existing[rc.ID] = true
	}
	for _, c := range combs {
		if existing[c.ID()] {
			continue
		}
		existing[c.ID()] = true
		r.Combinations = append(r.Combinations, r.newRunCombination(c))
	}
	r.Total = len(r.Combinations)
	r.recountLocked()
}

// newRunCombination builds the pending entry for one combination.
func (r *Run) newRunCombination(c Combination) RunCombination {
	return RunCombination{
		ID:        c.ID(),
		Identity:  fmt.Sprintf("%s/%s", r.ID, c.ID()),
		Toolchain: string(c.Lang),
		Version:   c.Version,
		OS:        c.OS,
		Arch:      c.Arch,
		Variant:   c.Variant,
		Platform:  c.Platform,
		Status:    "pending",
		Metadata:  c.Metadata,
	}
}

// RecordResult folds one completed combination result into the run: final
// status, duration, artifact, redacted error, and the full attempt history.
// Recording a result for an unknown combination ID is ignored (defensive:
// the executor only runs plan combinations). Counters track status
// transitions so a re-recorded combination is never double-counted.
func (r *Run) RecordResult(res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.Combinations {
		if r.Combinations[i].ID != res.Combination.ID() {
			continue
		}
		rc := &r.Combinations[i]
		r.uncountLocked(rc.Status)
		outcome := r.combinationEntry(res)
		// Only the outcome fields change; the entry keeps its recorded
		// dimensions, identity, and attempt start time.
		rc.Status = outcome.Status
		rc.Duration = outcome.Duration
		rc.Artifact = outcome.Artifact
		rc.SHA256 = outcome.SHA256
		rc.Error = outcome.Error
		rc.CacheStatus = outcome.CacheStatus
		rc.Attempts = outcome.Attempts
		rc.AttemptLog = outcome.AttemptLog
		r.countLocked(res.Status)
		return
	}
}

// MarkRunning records that one attempt of a combination started executing:
// the entry flips to "running" (still counted as incomplete/resumable), the
// in-flight attempt number and start time are stamped, and any previous
// outcome of a re-executed combination is cleared — a retried or resumed
// combination must never keep a stale artifact or checksum from an earlier
// attempt. Recording an unknown combination ID is ignored (defensive).
func (r *Run) MarkRunning(id string, attempt int, started time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.Combinations {
		rc := &r.Combinations[i]
		if rc.ID != id {
			continue
		}
		switch rc.Status {
		case "success", "failed", "skipped":
			// Terminal entries are never re-executed by the engine (resume
			// selects only incomplete combinations); ignore defensively so a
			// stray mark cannot rewrite history.
			return
		}
		r.uncountLocked(rc.Status)
		rc.Status = "running"
		rc.StartedAt = started
		rc.Duration = ""
		rc.Artifact = ""
		rc.SHA256 = ""
		rc.Error = ""
		rc.CacheStatus = ""
		if attempt > 0 {
			rc.Attempts = attempt
		}
		r.countLocked(rc.Status)
		return
	}
}

// combinationEntry converts an executor result into its RunCombination
// record (identity fields come from the caller's context).
func (r *Run) combinationEntry(res Result) RunCombination {
	rc := RunCombination{
		Status:      res.Status,
		Duration:    res.Duration.Round(time.Millisecond).String(),
		Artifact:    res.Artifact,
		SHA256:      res.SHA256,
		CacheStatus: res.CacheStatus,
		Attempts:    len(res.Attempts),
	}
	if rc.Attempts == 0 && res.Status != "" {
		rc.Attempts = 1
	}
	if res.Error != nil {
		rc.Error = phelixerr.Redact(res.Error.Error())
	}
	for _, a := range res.Attempts {
		entry := RunAttempt{
			Number:   a.Number,
			Status:   a.Status,
			Duration: a.Duration.Round(time.Millisecond).String(),
		}
		if a.Error != nil {
			entry.Error = phelixerr.Redact(a.Error.Error())
		}
		rc.AttemptLog = append(rc.AttemptLog, entry)
	}
	return rc
}

// uncountLocked/countLocked maintain the status counters across transitions,
// including the pending/running → terminal transition (which lowers Incomplete).
func (r *Run) uncountLocked(status string) {
	switch status {
	case "success":
		r.Succeeded--
	case "failed":
		r.Failed--
	case "skipped":
		r.Skipped--
	default: // pending, running, empty
		r.Incomplete--
	}
}

func (r *Run) countLocked(status string) {
	switch status {
	case "success":
		r.Succeeded++
	case "failed":
		r.Failed++
	case "skipped":
		r.Skipped++
	default: // pending, running, empty
		r.Incomplete++
	}
}

// recountLocked recomputes all counters from the combination entries.
func (r *Run) recountLocked() {
	r.Succeeded, r.Failed, r.Skipped, r.Incomplete = 0, 0, 0, 0
	for _, rc := range r.Combinations {
		switch rc.Status {
		case "success":
			r.Succeeded++
		case "failed":
			r.Failed++
		case "skipped":
			r.Skipped++
		default: // pending, running
			r.Incomplete++
		}
	}
}

// Finalize computes the overall status from the combination entries and
// stamps the finish time:
//
//	any pending/running combination        → interrupted (resumable)
//	otherwise no failures                  → succeeded
//	otherwise nothing succeeded            → failed
//	otherwise                              → partial
//
// An empty run is failed, exactly like Finish always was.
func (r *Run) Finalize(finished time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.FinishedAt = finished
	r.Duration = finished.Sub(r.StartedAt).Round(time.Second).String()
	r.Total = len(r.Combinations)
	r.recountLocked()
	r.computeStatusLocked()
}

// computeStatusLocked derives the overall run status from the counters.
// Caller holds mu.
func (r *Run) computeStatusLocked() {
	switch {
	case r.Total == 0:
		// A run that executed zero combinations did not succeed. ParsePlan
		// guarantees at least one combination, so this only guards degenerate
		// callers.
		r.Status = RunStatusFailed
	case r.Incomplete > 0:
		r.Status = RunStatusInterrupted
	case r.Failed == 0:
		r.Status = RunStatusSucceeded
	case r.Succeeded == 0:
		r.Status = RunStatusFailed
	default:
		r.Status = RunStatusPartial
	}
}

// Resumable reports whether the run has incomplete combinations that a
// resume would execute. Runs with status running (orphaned after a crash) and
// interrupted both qualify.
func (r *Run) Resumable() bool {
	if r == nil {
		return false
	}
	if IsTerminalRunStatus(r.Status) {
		return false
	}
	for _, rc := range r.Combinations {
		switch rc.Status {
		case "pending", "running", "":
			return true
		}
	}
	return false
}

// IncompleteCombinations returns the combinations a resume would execute:
// pending entries plus "running" entries orphaned by an interruption (their
// worker is gone — the run lock guarantees no live execution).
func (r *Run) IncompleteCombinations() []Combination {
	out := make([]Combination, 0)
	for _, rc := range r.Combinations {
		switch rc.Status {
		case "pending", "running", "":
			out = append(out, rc.Combination())
		}
	}
	return out
}

// FailedCombinations returns the combinations whose final status is failed —
// the selection `phelix matrix retry <run-id> --failed` executes.
func (r *Run) FailedCombinations() []Combination {
	out := make([]Combination, 0)
	for _, rc := range r.Combinations {
		if rc.Status == "failed" {
			out = append(out, rc.Combination())
		}
	}
	return out
}

// Finish folds the executor results into the run: per-combination entries
// (each carrying the run-scoped identity), counters, and the overall status.
// It is the single-shot lifecycle — one entry per result, appended in order,
// without deduplication (result lists come from a plan, which is already
// deduplicated; tests may pass arbitrary lists). Incremental executions use
// InitCombinations + RecordResult + Finalize instead.
//
// Status mapping: no failures → succeeded; some of each → partial; everything
// failed → failed. Errors are redacted the same way the JSON build report
// redacts them.
func (r *Run) Finish(results []Result, finished time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.FinishedAt = finished
	r.Duration = finished.Sub(r.StartedAt).Round(time.Second).String()
	r.Total = len(results)
	r.Combinations = make([]RunCombination, 0, len(results))

	for _, res := range results {
		rc := RunCombination{
			ID:        res.Combination.ID(),
			Identity:  fmt.Sprintf("%s/%s", r.ID, res.Combination.ID()),
			Toolchain: string(res.Combination.Lang),
			Version:   res.Combination.Version,
			OS:        res.Combination.OS,
			Arch:      res.Combination.Arch,
			Variant:   res.Combination.Variant,
			Platform:  res.Combination.Platform,
			Metadata:  res.Combination.Metadata,
		}
		outcome := r.combinationEntry(res)
		rc.Status = outcome.Status
		rc.Duration = outcome.Duration
		rc.Artifact = outcome.Artifact
		rc.SHA256 = outcome.SHA256
		rc.Error = outcome.Error
		rc.CacheStatus = outcome.CacheStatus
		rc.Attempts = outcome.Attempts
		rc.AttemptLog = outcome.AttemptLog
		r.Combinations = append(r.Combinations, rc)
	}

	r.recountLocked()
	r.computeStatusLocked()
}

// StatusCounters is an internally consistent per-status snapshot of a run's
// combinations. It always satisfies Total == Succeeded+Failed+Skipped+Running+
// Pending because it is computed from the combination entries in one pass
// under the run's lock — never from separately-updated fields that could be
// observed mid-transition.
type StatusCounters struct {
	Total     int `json:"total"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	Running   int `json:"running"`
	Pending   int `json:"pending"`
}

// Complete reports how many combinations have a final outcome (succeeded,
// failed, or skipped).
func (c StatusCounters) Complete() int {
	return c.Succeeded + c.Failed + c.Skipped
}

// SnapshotCounters returns the per-status combination counts as one
// consistent snapshot. It is the single source for aggregate counters in
// `matrix status` and anywhere else that renders live state — there is no
// second, separately-maintained counter system.
func (r *Run) SnapshotCounters() StatusCounters {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.countersLocked()
}

// countersLocked computes the counters; caller holds mu.
func (r *Run) countersLocked() StatusCounters {
	c := StatusCounters{Total: len(r.Combinations)}
	for _, rc := range r.Combinations {
		switch rc.Status {
		case "success":
			c.Succeeded++
		case "failed":
			c.Failed++
		case "skipped":
			c.Skipped++
		case "running":
			c.Running++
		default: // pending, empty
			c.Pending++
		}
	}
	return c
}

// Clone returns a deep copy of the run that is safe to read (and marshal)
// while executor workers keep mutating the original: persistence paths must
// never serialize a run that another goroutine is updating mid-field.
// Unexported state (the mutex) is not copied — the clone is a value snapshot.
func (r *Run) Clone() *Run {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := &Run{
		ID:          r.ID,
		AppName:     r.AppName,
		ProjectDir:  r.ProjectDir,
		Status:      r.Status,
		Config:      cloneRunConfig(r.Config),
		StartedAt:   r.StartedAt,
		FinishedAt:  r.FinishedAt,
		Duration:    r.Duration,
		Total:       r.Total,
		Succeeded:   r.Succeeded,
		Failed:      r.Failed,
		Skipped:     r.Skipped,
		Incomplete:  r.Incomplete,
		ParentRunID: r.ParentRunID,
		ResumeCount: r.ResumeCount,
	}
	if r.Combinations != nil {
		out.Combinations = make([]RunCombination, len(r.Combinations))
		for i, rc := range r.Combinations {
			rc.Metadata = copyStringMap(rc.Metadata)
			rc.AttemptLog = append([]RunAttempt(nil), rc.AttemptLog...)
			out.Combinations[i] = rc
		}
	}
	return out
}

// cloneRunConfig deep-copies the configuration snapshot.
func cloneRunConfig(cfg RunConfig) RunConfig {
	cfg.Versions = append([]string(nil), cfg.Versions...)
	cfg.Platforms = append([]string(nil), cfg.Platforms...)
	cfg.BuildArgs = append([]string(nil), cfg.BuildArgs...)
	cfg.Include = cloneRules(cfg.Include)
	cfg.Exclude = cloneRules(cfg.Exclude)
	return cfg
}

// cloneRules deep-copies rule slices (their dimension maps included).
func cloneRules(rules []Rule) []Rule {
	if rules == nil {
		return nil
	}
	out := make([]Rule, len(rules))
	for i, rule := range rules {
		rule.Dimensions = Dimensions(copyStringMap(rule.Dimensions))
		rule.Metadata = copyStringMap(rule.Metadata)
		out[i] = rule
	}
	return out
}

// copyStringMap returns a copy of m (nil stays nil, so omitempty semantics
// are preserved through a clone).
func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
