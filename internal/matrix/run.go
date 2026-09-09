package matrix

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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

// Run statuses. Only the states the local CLI executor can actually produce
// exist; there is deliberately no "cancelled" until the engine supports
// cancellation.
type RunStatus string

const (
	RunStatusRunning   RunStatus = "running"
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusPartial   RunStatus = "partial"
	RunStatusFailed    RunStatus = "failed"
)

// RunConfig is the snapshot of the effective matrix configuration a run
// executed with. It is captured at run start and never re-read from
// phelix.yaml, so later configuration edits cannot rewrite what an old run
// claims to have built.
type RunConfig struct {
	Lang        string   `json:"language"`
	Versions    []string `json:"versions"`
	Platforms   []string `json:"platforms"`
	Concurrency int      `json:"concurrency"`
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
}

// RunCombination is one combination's outcome inside a Run, carrying enough
// identity (run ID + all dimensions) to relate it unambiguously to its
// artifact and build result.
type RunCombination struct {
	ID          string `json:"id"`       // combination ID, e.g. "go1.26-linux-amd64"
	Identity    string `json:"identity"` // run-scoped identity: "<runID>/<combinationID>"
	Toolchain   string `json:"toolchain"`
	Version     string `json:"toolchain_version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Variant     string `json:"variant,omitempty"`
	Platform    string `json:"platform"`
	Status      string `json:"status"` // "success" | "failed" | "skipped"
	Duration    string `json:"duration"`
	Artifact    string `json:"artifact,omitempty"`
	Error       string `json:"error,omitempty"`
	CacheStatus string `json:"cache_status,omitempty"`
}

// Run is the logical wrapper around one matrix execution: it aggregates the
// run ID, the configuration snapshot, timing, per-combination results, and
// the overall status. It is the entity the local history persists and the
// future backend/gRPC phase will adopt as-is.
type Run struct {
	ID           RunID            `json:"id"`
	AppName      string           `json:"app_name"`
	ProjectDir   string           `json:"project_dir,omitempty"`
	Status       RunStatus        `json:"status"`
	Config       RunConfig        `json:"config"`
	StartedAt    time.Time        `json:"started_at"`
	FinishedAt   time.Time        `json:"finished_at,omitempty"`
	Duration     string           `json:"duration,omitempty"`
	Total        int              `json:"total"`
	Succeeded    int              `json:"succeeded"`
	Failed       int              `json:"failed"`
	Skipped      int              `json:"skipped"`
	Combinations []RunCombination `json:"combinations"`
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
			Sources: RunConfigSources{
				Lang:        prof.Source.Lang,
				Versions:    prof.Source.Versions,
				Platforms:   prof.Source.Platforms,
				Concurrency: prof.Source.Concurrency,
			},
		}
	}
	return r
}

// Finish folds the executor results into the run: per-combination entries
// (each carrying the run-scoped identity), counters, and the overall status.
// Status mapping: no failures → succeeded; some of each → partial; everything
// failed → failed. Errors are redacted the same way the JSON build report
// redacts them.
func (r *Run) Finish(results []Result, finished time.Time) {
	r.FinishedAt = finished
	r.Duration = finished.Sub(r.StartedAt).Round(time.Second).String()
	r.Total = len(results)
	r.Combinations = make([]RunCombination, 0, len(results))

	for _, res := range results {
		rc := RunCombination{
			ID:          res.Combination.ID(),
			Identity:    fmt.Sprintf("%s/%s", r.ID, res.Combination.ID()),
			Toolchain:   string(res.Combination.Lang),
			Version:     res.Combination.Version,
			OS:          res.Combination.OS,
			Arch:        res.Combination.Arch,
			Variant:     res.Combination.Variant,
			Platform:    res.Combination.Platform,
			Status:      res.Status,
			Duration:    res.Duration.Round(time.Millisecond).String(),
			Artifact:    res.Artifact,
			CacheStatus: res.CacheStatus,
		}
		if res.Error != nil {
			rc.Error = phelixerr.Redact(res.Error.Error())
		}
		r.Combinations = append(r.Combinations, rc)

		switch res.Status {
		case "success":
			r.Succeeded++
		case "failed":
			r.Failed++
		case "skipped":
			r.Skipped++
		}
	}

	switch {
	case r.Total == 0:
		// A run that executed zero combinations did not succeed. ParsePlan
		// guarantees at least one combination, so this only guards degenerate
		// callers.
		r.Status = RunStatusFailed
	case r.Failed == 0:
		r.Status = RunStatusSucceeded
	case r.Succeeded == 0:
		r.Status = RunStatusFailed
	default:
		r.Status = RunStatusPartial
	}
}
