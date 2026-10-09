package authz

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Approval is the Phase 4 approval primitive, and it is deliberately the
// smallest useful one:
//
//	this exact approver approved this exact plan
//
// There is no workflow, no step machine, no actor group, no quorum, no
// escalation, no notification and no deadline — those belong to a later phase
// if they are ever justified. What exists here is a durable artifact bound to
// an immutable plan, and that is enough to make "approval required" a real
// execution gate instead of a label.
//
// Immutability is structural, exactly as for plans: the file is created
// exactly once (create-only link), its content is covered by a canonical
// hash, and every load re-verifies that hash. An approval is never edited —
// a changed decision means revoking it and approving again.
type Approval struct {
	SchemaVersion string `json:"schema_version"`
	ApprovalID    string `json:"approval_id"`
	// ApprovalHash covers exactly the binding fields below, so hand-editing
	// any of them is detected on load (APPROVAL_INVALID).
	ApprovalHash string `json:"approval_hash"`

	// The execution binding. All four fields must match the plan being
	// applied; see PolicyAuthorizer.decideWithApproval.
	PlanID   string `json:"plan_id"`
	PlanHash string `json:"plan_hash"`
	Action   string `json:"action"`
	Target   string `json:"target"`

	// Decision is always "approved". A refusal is not stored: the absence of
	// an approval IS the refusal, which keeps the artifact set unambiguous
	// (no "approved: false" that a later bug could read as present).
	Decision string `json:"decision"`

	// Approver records what was actually proven about whoever approved, never
	// a claimed identity. In Phase 4 that is filesystem authority over this
	// host's Phelix data directory, reported as an unauthenticated CLI actor
	// with ApproverSource "local_host".
	ApproverType          string `json:"approver_type"`
	ApproverID            string `json:"approver_id,omitempty"`
	ApproverAuthenticated bool   `json:"approver_authenticated"`
	ApproverSource        string `json:"approver_source"`
	// ApproverProvenance is the unverified host login name of the process
	// that created the approval. Audit colour only: nothing matches on it and
	// it grants nothing.
	ApproverProvenance string `json:"approver_provenance,omitempty"`

	ApprovedAtMs int64 `json:"approved_at_ms"`
}

// DecisionApproved is the only stored approval decision.
const DecisionApproved = "approved"

// ApproverSourceLocalHost names what a Phase 4 approval actually proves:
// write access to this host's Phelix data directory. It is recorded honestly
// rather than dressed up as an authenticated principal.
const ApproverSourceLocalHost = "local_host"

// approvalIDPrefix — apr_ + 16 hex chars, mirroring the plan/operation ID
// discipline.
const approvalIDPrefix = "apr_"

var approvalIDPattern = regexp.MustCompile(`^apr_[0-9a-f]{16}$`)

// maxApprovalBytes bounds a persisted approval. An approval is a binding, not
// a document.
const maxApprovalBytes = 8 * 1024

// ApprovalsDir returns the approval store directory.
func ApprovalsDir() string { return filepath.Join(Dir(), "approvals") }

// approvalPath returns the on-disk path for a plan's approval, after
// validating the plan ID shape so an ID can never traverse the filesystem.
// Filing approvals by plan ID is itself part of the binding: a lookup for
// plan B physically cannot return plan A's artifact.
func approvalPath(planID string) (string, error) {
	if !planIDPattern.MatchString(planID) {
		return "", invalidf("invalid plan id %q", planID)
	}
	return filepath.Join(ApprovalsDir(), planID+".json"), nil
}

// planIDPattern mirrors internal/plans' ID shape. It is duplicated rather
// than imported because authz must not depend on the plan package: the
// authorization layer works on plan REFERENCES, never on plan content.
var planIDPattern = regexp.MustCompile(`^pln_[0-9a-f]{16}$`)

// newApprovalID mints an approval ID.
func newApprovalID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "generate approval id", err)
	}
	return approvalIDPrefix + hex.EncodeToString(buf), nil
}

// approvalHashInput is the canonical hash preimage: exactly the binding and
// approver fields. approval_id and approved_at are excluded for the same
// reason plan hashing excludes plan_id and created_at — they are identity and
// metadata, not what was approved.
type approvalHashInput struct {
	SchemaVersion         string
	PlanID                string
	PlanHash              string
	Action                string
	Target                string
	Decision              string
	ApproverType          string
	ApproverID            string
	ApproverAuthenticated bool
	ApproverSource        string
}

// Hash computes the canonical approval hash.
func (a *Approval) Hash() (string, error) {
	if a == nil {
		return "", invalidf("nil approval")
	}
	data, err := json.Marshal(approvalHashInput{
		SchemaVersion:         a.SchemaVersion,
		PlanID:                a.PlanID,
		PlanHash:              a.PlanHash,
		Action:                a.Action,
		Target:                a.Target,
		Decision:              a.Decision,
		ApproverType:          a.ApproverType,
		ApproverID:            a.ApproverID,
		ApproverAuthenticated: a.ApproverAuthenticated,
		ApproverSource:        a.ApproverSource,
	})
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeApprovalInvalid, "canonicalize approval", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyHash re-derives the canonical hash and compares it with the stored
// value. A mismatch means the artifact was corrupted or edited after
// creation — for instance to re-point it at a different plan — and the
// approval is refused.
//
// This detects corruption and casual tampering. It is deliberately not a
// signature: Phase 4 has no key material and inventing one would be fake
// cryptographic authentication. An attacker who can write the data directory
// can equally rewrite apps.json or the plan store, so the honest trust
// boundary is the data directory itself.
func (a *Approval) VerifyHash() error {
	computed, err := a.Hash()
	if err != nil {
		return err
	}
	if computed != a.ApprovalHash {
		return phelixerr.Newf(phelixerr.CodeApprovalInvalid,
			"approval content changed after creation (hash mismatch); approvals are immutable — revoke it and approve again")
	}
	return nil
}

// NewApproval builds and finalizes an approval for one plan. It does not
// persist: [SaveApproval] does, exactly once.
func NewApproval(approver Actor, plan PlanRef, action, target, provenance string) (*Approval, error) {
	if plan.ID == "" || plan.Hash == "" {
		return nil, invalidf("an approval requires both a plan id and a plan hash")
	}
	switch action {
	case ActionRebuild, ActionRollback:
	default:
		return nil, invalidf("unknown approvable action %q", action)
	}
	if target == "" {
		return nil, invalidf("an approval requires a target application")
	}
	if err := approver.Validate(); err != nil {
		return nil, err
	}
	id, err := newApprovalID()
	if err != nil {
		return nil, err
	}
	a := &Approval{
		SchemaVersion:         SchemaVersion,
		ApprovalID:            id,
		PlanID:                plan.ID,
		PlanHash:              plan.Hash,
		Action:                action,
		Target:                target,
		Decision:              DecisionApproved,
		ApproverType:          approver.Type,
		ApproverID:            approver.ID,
		ApproverAuthenticated: approver.Authenticated,
		ApproverSource:        ApproverSourceLocalHost,
		ApproverProvenance:    phelixerr.Redact(provenance),
		ApprovedAtMs:          time.Now().UnixMilli(),
	}
	hash, err := a.Hash()
	if err != nil {
		return nil, err
	}
	a.ApprovalHash = hash
	return a, nil
}

// SaveApproval persists an approval exactly once. The write is atomic
// (tmp + fsync + rename-by-link) and create-only, so an existing approval can
// never be silently replaced — re-approving a plan is ALREADY_EXISTS, and
// changing one's mind means revoking first.
func SaveApproval(a *Approval) error {
	if a == nil {
		return invalidf("nil approval")
	}
	if err := a.VerifyHash(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeApprovalInvalid, "encode approval", err)
	}
	if len(data) > maxApprovalBytes {
		return invalidf("approval is %d bytes; the maximum is %d", len(data), maxApprovalBytes)
	}
	dir := ApprovalsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create approval store directory", err)
	}
	final, err := approvalPath(a.PlanID)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return phelixerr.Newf(phelixerr.CodeAlreadyExists,
			"plan %s is already approved — approvals are immutable; revoke it first with 'phelix authz revoke'", a.PlanID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "stat approval", err)
	}

	tmp := fmt.Sprintf("%s.tmp-%d", final, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "create approval temp file", err)
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "write approval", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync approval", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "close approval", err)
	}
	// os.Link is atomic create-only: a concurrent approver loses the race
	// instead of overwriting the first approval.
	if err := os.Link(tmp, final); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return phelixerr.Newf(phelixerr.CodeAlreadyExists,
				"plan %s is already approved — approvals are immutable; revoke it first with 'phelix authz revoke'", a.PlanID)
		}
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "persist approval", err)
	}
	_ = os.Remove(tmp)
	d, err := os.Open(dir)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "open approval store directory", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "sync approval store directory", err)
	}
	return nil
}

// LoadApproval reads and fully verifies the approval filed for a plan.
// A missing approval is (nil, nil) — absence is a normal state, not an error.
func LoadApproval(planID string) (*Approval, error) {
	path, err := approvalPath(planID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeAuthzUnavailable, "read approval", err)
	}
	if len(data) > maxApprovalBytes {
		return nil, phelixerr.Newf(phelixerr.CodeApprovalInvalid,
			"approval for plan %s is oversized", planID)
	}
	a, err := DecodeApproval(data)
	if err != nil {
		return nil, err
	}
	// The artifact's own plan ID must agree with the file it was found under:
	// copying plan A's approval over plan B's path does not authorize plan B.
	if a.PlanID != planID {
		return nil, phelixerr.Newf(phelixerr.CodeApprovalInvalid,
			"approval filed for plan %s is bound to plan %s", planID, a.PlanID)
	}
	return a, nil
}

// DecodeApproval parses and fully verifies approval bytes.
func DecodeApproval(data []byte) (*Approval, error) {
	var a Approval
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeApprovalInvalid, "approval is not a parseable approval", err)
	}
	if a.SchemaVersion != SchemaVersion {
		return nil, phelixerr.Newf(phelixerr.CodeApprovalInvalid,
			"approval schema_version %q, want %q", a.SchemaVersion, SchemaVersion)
	}
	if !approvalIDPattern.MatchString(a.ApprovalID) {
		return nil, phelixerr.Newf(phelixerr.CodeApprovalInvalid, "approval has malformed id %q", a.ApprovalID)
	}
	if a.Decision != DecisionApproved {
		return nil, phelixerr.Newf(phelixerr.CodeApprovalInvalid,
			"approval has unknown decision %q", a.Decision)
	}
	if err := a.VerifyHash(); err != nil {
		return nil, err
	}
	return &a, nil
}

// RevokeApproval removes a plan's approval. Revocation deletes the artifact;
// it never edits one, so the immutability of what was approved is preserved.
// Returns false when there was nothing to revoke.
func RevokeApproval(planID string) (bool, error) {
	path, err := approvalPath(planID)
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, phelixerr.Wrap(phelixerr.CodeFilesystem, "revoke approval", err)
	}
	return true, nil
}

// ResolveApproval turns the stored approval (or its absence, or its
// unreadability) into the [ApprovalState] the authorizer decides on. A load
// failure becomes a recorded Err rather than a returned error, because "the
// approval is broken" is a denial the boundary must report precisely — not an
// internal failure that could be mistaken for a transient one.
func ResolveApproval(planID string) ApprovalState {
	a, err := LoadApproval(planID)
	if err != nil {
		return ApprovalState{Present: true, Err: err}
	}
	if a == nil {
		return ApprovalState{}
	}
	return ApprovalState{
		Present:    true,
		ApprovalID: a.ApprovalID,
		Binding: ApprovalBinding{
			PlanID:   a.PlanID,
			PlanHash: a.PlanHash,
			Action:   a.Action,
			Target:   a.Target,
		},
	}
}
