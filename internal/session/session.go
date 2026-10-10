// Package session implements Phelix's durable agent execution sessions: a
// small, versioned, auditable record that groups ONE unit of external-agent
// work across inspect → plan → authorize → apply → verify, so an interrupted
// agent can resume by READING durable state.
//
// A session is a tracking primitive, deliberately NOT an execution principal:
//   - It references existing plans, operations and deployments by id; it never
//     copies their authoritative state and is never a second source of truth.
//   - Creating or modifying a session confers NO authorization and executes
//     nothing. Deployment stays behind the Phase 4 boundary (plan apply).
//   - Its lifecycle (active → completed|failed|cancelled) is independent of any
//     operation or deployment outcome; a terminal session never re-activates.
//
// Persistence mirrors the plan/operation stores (atomic create-only create,
// atomic-replace updates under a per-session OS file lock, id-validated paths,
// bounded size, fail-closed loads). The event history is append-only within
// the record and ordered by a monotonic seq assigned under the lock; it is
// honest provenance, NOT a cryptographically authenticated audit log — the
// data directory is the trust boundary (as with Phase 4 approvals).
package session

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
)

// SchemaVersion is the session-record schema version. A load of any other
// version fails closed with SESSION_INVALID rather than guessing.
const SchemaVersion = "1"

// IDPrefix — session IDs are ses_ + 16 hex chars (8 crypto/rand bytes),
// matching the plan/operation/webhook id disciplines.
const IDPrefix = "ses_"

var idPattern = regexp.MustCompile(`^ses_[0-9a-f]{16}$`)

// Lifecycle statuses. active is the only non-terminal state; the three
// terminal states never transition anywhere (a session is never silently
// re-activated).
const (
	StatusActive    = "active"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Event types recorded in the append-only history (one event per mutating
// command).
const (
	EventCreated    = "created"
	EventCheckpoint = "checkpoint"
	EventCompleted  = "completed"
	EventFailed     = "failed"
	EventCancelled  = "cancelled"
)

// Bounds on client-supplied content. Over-cap input is REJECTED with
// INVALID_ARGUMENT, never silently clamped — matching the inspect/limit
// philosophy, so an agent is told rather than quietly truncated.
const (
	MaxTitleLen       = 200
	MaxProjectLen     = 200
	MaxAppLen         = 128
	MaxStepLen        = 120
	MaxNoteLen        = 500
	MaxFinalResultLen = 2000
	MaxRefsPerKind    = 50
	MaxEvents         = 200
	// MaxSessionBytes bounds a persisted session so the tracking record can
	// never become a dumping ground. Field caps keep a realistic session far
	// below this; the total is a defensive backstop checked at write time.
	MaxSessionBytes = 256 * 1024
)

// Session is one durable agent execution session.
type Session struct {
	SchemaVersion string `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Status        string `json:"status"`
	Title         string `json:"title,omitempty"`
	Project       string `json:"project,omitempty"`
	App           string `json:"app,omitempty"`
	// Actor is provenance metadata only (cli or mcp), derived server-side from
	// the authenticator — never from client input, and never a privilege.
	Actor *machine.Actor `json:"actor,omitempty"`
	// Rev is a monotonic revision bumped on every successful mutating
	// transition; it is the optimistic-concurrency token (see Update).
	Rev       int   `json:"rev"`
	CreatedAt int64 `json:"created_at_ms"`
	UpdatedAt int64 `json:"updated_at_ms"`
	// Reference sets correlate existing entities by id. Append-only and
	// deduplicated; bounded to MaxRefsPerKind each. The session never stores a
	// copy of the referenced artifact's content.
	PlanIDs       []string `json:"plan_ids,omitempty"`
	OperationIDs  []string `json:"operation_ids,omitempty"`
	DeploymentIDs []string `json:"deployment_ids,omitempty"`
	// Checkpoint is the last-known workflow step (overwritten each checkpoint).
	Checkpoint *CheckpointInfo `json:"checkpoint,omitempty"`
	// FinalResult is a bounded completion summary, set once at a terminal
	// transition. It is a REPORTED outcome, not independently verified health.
	FinalResult string `json:"final_result,omitempty"`
	// Events is the append-only lifecycle history, ordered by Seq.
	Events []Event `json:"events"`
}

// CheckpointInfo is the last-recorded workflow position.
type CheckpointInfo struct {
	Step string `json:"step,omitempty"`
	Note string `json:"note,omitempty"`
	AtMs int64  `json:"at_ms"`
	Seq  int    `json:"seq"`
}

// Event is one entry in the append-only history. Seq is a monotonic, 0-based
// index assigned under the per-session lock, which is also how ordering is
// established and how concurrent writers are serialized.
type Event struct {
	Seq    int    `json:"seq"`
	Type   string `json:"type"`
	AtMs   int64  `json:"at_ms"`
	Detail string `json:"detail,omitempty"`
}

// NewID mints a new ses_ session ID.
func NewID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "generate session id", err)
	}
	return IDPrefix + hex.EncodeToString(buf), nil
}

// ValidateID reports whether id is a well-formed session ID.
func ValidateID(id string) bool { return idPattern.MatchString(id) }

// validStatus reports whether s is a known lifecycle status.
func validStatus(s string) bool {
	switch s {
	case StatusActive, StatusCompleted, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// IsTerminal reports whether status is a terminal lifecycle state.
func IsTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

// ValidStatus reports whether s is a known lifecycle status. It is exported for
// the command layer to validate a --status filter before querying.
func ValidStatus(s string) bool { return validStatus(s) }

// boundedText validates a client-supplied free-text field against its cap
// (rejecting, not clamping) and returns it redacted so neither persistence nor
// any output can carry a secret. Length is measured on the raw input.
func boundedText(field, value string, max int) (string, error) {
	if len(value) > max {
		return "", phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"%s is %d bytes; the maximum is %d", field, len(value), max)
	}
	return phelixerr.Redact(value), nil
}

// ensureActive rejects a mutating transition on a non-active session with
// SESSION_INVALID_TRANSITION, leaving the session untouched.
func (s *Session) ensureActive(action string) error {
	if s.Status != StatusActive {
		return phelixerr.Newf(phelixerr.CodeSessionInvalidTransition,
			"session %s is %s; %q is only valid while the session is active", s.SessionID, s.Status, action)
	}
	return nil
}

// appendEvent appends one ordered event. Seq is len(Events) so it is monotonic
// and gap-free; callers hold the per-session lock, so ordering is deterministic.
func (s *Session) appendEvent(typ, detail string) {
	s.Events = append(s.Events, Event{
		Seq:    len(s.Events),
		Type:   typ,
		AtMs:   time.Now().UnixMilli(),
		Detail: detail,
	})
}

// addRef adds id to a bounded, deduplicated reference list. A duplicate is a
// no-op (added=false, no error); exceeding MaxRefsPerKind is INVALID_ARGUMENT.
func addRef(list []string, id string) ([]string, bool, error) {
	for _, x := range list {
		if x == id {
			return list, false, nil
		}
	}
	if len(list) >= MaxRefsPerKind {
		return list, false, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"too many references of one kind (max %d)", MaxRefsPerKind)
	}
	return append(list, id), true, nil
}
