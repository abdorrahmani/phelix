package session

import (
	"regexp"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/plans"
)

// This file is the session application layer: the verbs the CLI command group
// and the MCP tools both call. It validates and redacts client input, enforces
// the lifecycle transition table, resolves references through the existing
// plan/operation read APIs, and delegates persistence to the store. It never
// executes a plan, mutates a deployment, or touches the authorization boundary
// — a session is a tracking record, not an execution principal.

// CreateOpts are the inputs to Create. Actor is provenance derived server-side
// (never from client input).
type CreateOpts struct {
	Title   string
	Project string
	App     string
	Actor   *machine.Actor
}

// Refs is a set of references a mutating op may attach to a session.
type Refs struct {
	Plans       []string
	Operations  []string
	Deployments []string
}

func (r Refs) empty() bool {
	return len(r.Plans) == 0 && len(r.Operations) == 0 && len(r.Deployments) == 0
}

// CheckpointOpts records a bounded workflow checkpoint and/or attaches
// references. ExpectRev < 0 skips the optimistic-concurrency check.
type CheckpointOpts struct {
	Step      string
	Note      string
	Refs      Refs
	ExpectRev int
}

// CompleteOpts / FailOpts / CancelOpts drive the terminal transitions.
type CompleteOpts struct {
	Result    string
	Refs      Refs
	ExpectRev int
}

type FailOpts struct {
	Reason    string
	Refs      Refs
	ExpectRev int
}

type CancelOpts struct {
	Reason    string
	ExpectRev int
}

// Create starts a new active session. It executes nothing and creates no
// deployment — it only writes a tracking record.
func Create(opts CreateOpts) (*Session, error) {
	title, err := boundedText("title", opts.Title, MaxTitleLen)
	if err != nil {
		return nil, err
	}
	project, err := boundedText("project", opts.Project, MaxProjectLen)
	if err != nil {
		return nil, err
	}
	if len(opts.App) > MaxAppLen {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"app is %d bytes; the maximum is %d", len(opts.App), MaxAppLen)
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	s := &Session{
		SchemaVersion: SchemaVersion,
		SessionID:     id,
		Status:        StatusActive,
		Title:         title,
		Project:       project,
		App:           opts.App,
		Actor:         opts.Actor,
		Rev:           1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	s.appendEvent(EventCreated, "session created")
	if err := insert(s); err != nil {
		return nil, err
	}
	return s, nil
}

// Show loads a session and, when resolve is set, resolves its references
// through the existing read APIs. Resolution is read-only and bounded.
func Show(id string, resolve bool) (*SessionResult, error) {
	s, err := Load(id)
	if err != nil {
		return nil, err
	}
	res := &SessionResult{Session: s}
	if resolve {
		res.Resolved = Resolve(s)
	}
	return res, nil
}

// Checkpoint records a bounded workflow checkpoint and/or attaches references,
// while the session is active. It is NOT an authorization or execution
// instruction — free text is stored as provenance only.
func Checkpoint(id string, opts CheckpointOpts) (*Session, error) {
	if opts.Step == "" && opts.Note == "" && opts.Refs.empty() {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument,
			"nothing to checkpoint: supply a note, a step, or at least one reference")
	}
	step, err := boundedText("step", opts.Step, MaxStepLen)
	if err != nil {
		return nil, err
	}
	note, err := boundedText("note", opts.Note, MaxNoteLen)
	if err != nil {
		return nil, err
	}
	if err := validateRefs(opts.Refs); err != nil {
		return nil, err
	}
	return update(id, opts.ExpectRev, func(s *Session) error {
		if err := s.ensureActive("checkpoint"); err != nil {
			return err
		}
		if len(s.Events) >= MaxEvents {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"session event history is full (max %d); create a new session to continue tracking", MaxEvents)
		}
		linked, err := applyRefs(s, opts.Refs)
		if err != nil {
			return err
		}
		s.Checkpoint = &CheckpointInfo{Step: step, Note: note, AtMs: time.Now().UnixMilli(), Seq: len(s.Events)}
		s.appendEvent(EventCheckpoint, checkpointDetail(step, note, linked))
		return nil
	})
}

// Complete, Fail and Cancel are the terminal transitions. Each records a
// bounded reported outcome; none stops a deployment, cancels an operation or
// mutates an application. completed/failed is a REPORTED outcome, never a
// verified statement about runtime health.
func Complete(id string, opts CompleteOpts) (*Session, error) {
	return terminal(id, "complete", StatusCompleted, EventCompleted, opts.Result, opts.Refs, opts.ExpectRev)
}

func Fail(id string, opts FailOpts) (*Session, error) {
	return terminal(id, "fail", StatusFailed, EventFailed, opts.Reason, opts.Refs, opts.ExpectRev)
}

func Cancel(id string, opts CancelOpts) (*Session, error) {
	return terminal(id, "cancel", StatusCancelled, EventCancelled, opts.Reason, Refs{}, opts.ExpectRev)
}

// terminal validates the summary and references, then transitions an active
// session to a terminal state under the lock. A terminal transition always
// records its single closing event, even if the event history is at its cap.
func terminal(id, action, status, eventType, summary string, refs Refs, expectRev int) (*Session, error) {
	final, err := boundedText("summary", summary, MaxFinalResultLen)
	if err != nil {
		return nil, err
	}
	if err := validateRefs(refs); err != nil {
		return nil, err
	}
	return update(id, expectRev, func(s *Session) error {
		if err := s.ensureActive(action); err != nil {
			return err
		}
		linked, err := applyRefs(s, refs)
		if err != nil {
			return err
		}
		if final != "" {
			s.FinalResult = final
		}
		s.Status = status
		detail := strings.TrimSpace(strings.Join(nonEmpty(final, linked), "; "))
		if detail == "" {
			detail = "session " + status
		}
		s.appendEvent(eventType, detail)
		return nil
	})
}

// --- references -----------------------------------------------------------

var deploymentIDPattern = regexp.MustCompile(`^dep-[0-9a-f]{1,64}$`)

// validateRefs checks every reference BEFORE the lock (reads only), so a bad
// reference fails fast with INVALID_ARGUMENT and never opens the session for
// writing. Plans and operations must exist (and plans must hash-verify);
// deployment ids are format-validated here and resolved for existence at show
// time, because Phelix keeps no standalone deployment-id store.
func validateRefs(refs Refs) error {
	for _, id := range refs.Plans {
		if _, err := plans.Load(id); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeInvalidArgument, err, "cannot link plan %q", id)
		}
	}
	for _, id := range refs.Operations {
		if _, err := ops.Load(id); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeInvalidArgument, err, "cannot link operation %q", id)
		}
	}
	for _, id := range refs.Deployments {
		if !deploymentIDPattern.MatchString(id) {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"cannot link deployment %q: expected dep- followed by hex characters", id)
		}
	}
	return nil
}

// applyRefs adds validated references to s (deduped, bounded) and returns a
// short summary of what was newly linked, for the event detail.
func applyRefs(s *Session, refs Refs) (string, error) {
	var linked []string
	add := func(list []string, ids []string) ([]string, error) {
		for _, id := range ids {
			next, added, err := addRef(list, id)
			if err != nil {
				return nil, err
			}
			list = next
			if added {
				linked = append(linked, id)
			}
		}
		return list, nil
	}
	var err error
	if s.PlanIDs, err = add(s.PlanIDs, refs.Plans); err != nil {
		return "", err
	}
	if s.OperationIDs, err = add(s.OperationIDs, refs.Operations); err != nil {
		return "", err
	}
	if s.DeploymentIDs, err = add(s.DeploymentIDs, refs.Deployments); err != nil {
		return "", err
	}
	if len(linked) == 0 {
		return "", nil
	}
	return "linked " + strings.Join(linked, ", "), nil
}

func checkpointDetail(step, note, linked string) string {
	var parts []string
	if step != "" {
		parts = append(parts, "step="+step)
	}
	if note != "" {
		parts = append(parts, note)
	}
	if linked != "" {
		parts = append(parts, linked)
	}
	return strings.Join(parts, "; ")
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// opsListForResolve is the seam Resolve uses to enumerate operation records
// for deployment correlation. It exists so a test can assert that resolving K
// deployment references performs exactly ONE operation scan, not K. Production
// never replaces it.
var opsListForResolve = ops.List

// Resolve reports the current state of a session's references through the
// existing read APIs. It is bounded (reference sets are capped) and never
// fabricates state.
func Resolve(s *Session) *Resolved {
	r := &Resolved{
		Plans:       make([]RefView, 0, len(s.PlanIDs)),
		Operations:  make([]RefView, 0, len(s.OperationIDs)),
		Deployments: make([]RefView, 0, len(s.DeploymentIDs)),
	}
	for _, id := range s.PlanIDs {
		if p, err := plans.Load(id); err == nil {
			r.Plans = append(r.Plans, RefView{ID: id, Present: true, State: RefPresent, Status: p.Status, Detail: p.PlanHash})
		} else {
			r.Plans = append(r.Plans, refErr(id, err))
		}
	}
	for _, id := range s.OperationIDs {
		if rec, err := ops.Load(id); err == nil {
			r.Operations = append(r.Operations, RefView{ID: id, Present: true, State: RefPresent, Status: rec.Status, Detail: rec.Kind})
		} else {
			r.Operations = append(r.Operations, refErr(id, err))
		}
	}

	// Deployment references have no standalone store; each correlates with an
	// operation record by deployment id. Build that index with a SINGLE
	// operation scan and resolve every reference against it, instead of one
	// full scan per reference (K refs ⇒ K scans).
	var depByID map[string]*ops.Record
	var depScanErr error
	if len(s.DeploymentIDs) > 0 {
		recs, _, err := opsListForResolve("", 0)
		if err != nil {
			depScanErr = err
		} else {
			depByID = make(map[string]*ops.Record, len(recs))
			for _, rec := range recs {
				// List is newest-first; keep the first (newest) match, matching
				// ops.FindByDeploymentID's result.
				if rec.DeploymentID != "" {
					if _, seen := depByID[rec.DeploymentID]; !seen {
						depByID[rec.DeploymentID] = rec
					}
				}
			}
		}
	}
	for _, id := range s.DeploymentIDs {
		switch {
		case depScanErr != nil:
			r.Deployments = append(r.Deployments, refErr(id, depScanErr))
		case depByID[id] != nil:
			rec := depByID[id]
			r.Deployments = append(r.Deployments, RefView{ID: id, Present: true, State: RefPresent, Status: rec.Status, Detail: "operation " + rec.ID})
		default:
			// No correlating operation record: existence cannot be confirmed by
			// id (no standalone deployment-id store) — report unknown.
			r.Deployments = append(r.Deployments, RefView{ID: id, Present: false, State: RefUnknown})
		}
	}
	return r
}

func refErr(id string, err error) RefView {
	if phelixerr.CodeOf(err) == phelixerr.CodeNotFound {
		return RefView{ID: id, Present: false, State: RefMissing}
	}
	return RefView{ID: id, Present: false, State: RefUnavailable, ErrorCode: phelixerr.CodeOf(err).String()}
}
