// Package authz implements Phelix's execution authorization boundary: the
// single gate between a validated, immutable Plan and the existing execution
// engine.
//
// The question this package answers is narrow on purpose:
//
//	"Is this caller allowed to execute THIS exact plan, right now?"
//
// It never answers "what should be deployed" — that is the agent's reasoning —
// and it owns no deployment semantics: no build, deploy, health, version or
// rollback logic lives here, and nothing in this package mutates anything.
//
// The architectural invariant Phase 4 establishes:
//
//	request → plan → authorization → (approval) → execution
//
// and never request → execution for a protected path.
//
// Layering, and why it is shaped this way:
//
//	Caller (CLI | API | MCP | agent | service)
//	   ↓  Authenticator            — the ONLY producer of an Actor
//	Actor
//	   ↓  Request{actor, action, target, plan ref, approval state}
//	Authorizer                     — pure decision, no I/O, no flags, no headers
//	   ↓
//	Decision{allow | deny | approval_required} + stable code
//
// The authorizer receives an abstract Actor and a plan REFERENCE, never a
// transport object and never the Phase 2 context, so a future HTTP or MCP
// front end plugs in at the Authenticator seam without the authorization
// rules changing by a line.
//
// Fail-closed is the default posture for protected execution: an unreadable or
// invalid policy denies, it never degrades to "allow". The one deliberate
// exception is documented and explicit — see [Policy] and ModeLegacyLocal.
package authz

import (
	"context"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// SchemaVersion is the on-disk schema shared by the policy, approval and
// audit artifacts this package persists.
const SchemaVersion = "1"

// Authorizable actions. These are exactly the Phase 3 plan actions — the
// mutations that have an immutable plan to bind to. Authorization is never
// expressed over a vaguer verb than this.
const (
	ActionRebuild  = "rebuild"
	ActionRollback = "rollback"
)

// Decision effects. This is the complete vocabulary; there is no fourth
// outcome and no free-form decision text.
const (
	EffectAllow            = "allow"
	EffectDeny             = "deny"
	EffectApprovalRequired = "approval_required"
)

// PlanRef is the immutable execution binding an authorization decision is
// made against. Both fields are required for a protected decision: a plan ID
// alone is not an execution identity, because a plan ID says WHICH artifact
// and the hash says WHAT IT WILL DO. Authorizing by ID alone would let
// different content execute under an old decision.
type PlanRef struct {
	ID   string
	Hash string
}

// Target is what the mutation operates on. It is a projection — an app name
// and ID — not the Phase 2 context object: passing the whole context would
// couple the authorization layer to every state source Phelix has.
type Target struct {
	App   string
	AppID string
}

// Request is the complete input to a decision. It carries metadata and
// references only: no passwords, tokens, private keys or secret environment
// values ever enter this struct, so the authorization layer cannot become a
// secret-handling path (and its audit records cannot become a leak).
type Request struct {
	Actor  Actor
	Action string
	Target Target
	// Plan is the immutable plan the request is bound to. A nil Plan is a
	// PLANLESS request — a direct `phelix rebuild`/`rollback` invocation. It
	// is a legitimate input, and under enforced authorization it is always
	// denied: protected execution must be plan-bound.
	Plan *PlanRef
	// Approval is the approval state already resolved for this plan by the
	// boundary (see [ResolveApproval]). nil means "no approval artifact
	// exists". Keeping resolution outside the decision keeps the Authorizer
	// pure and testable.
	Approval *ApprovalState
}

// ApprovalState is the resolved, already-verified approval situation for one
// plan. Verification (schema, content hash, plan binding) happens during
// resolution, so the decision function only compares bindings.
type ApprovalState struct {
	// Present reports that an approval artifact exists for this plan.
	Present bool
	// ApprovalID of the artifact, when present.
	ApprovalID string
	// Err is the structured failure from loading or verifying the artifact
	// (APPROVAL_INVALID). A broken approval never counts as an approval.
	Err error
	// Binding is what the approval actually authorizes.
	Binding ApprovalBinding
}

// ApprovalBinding is the execution identity an approval is bound to. An
// approval authorizes exactly this tuple and nothing else.
type ApprovalBinding struct {
	PlanID   string
	PlanHash string
	Action   string
	Target   string
}

// Decision is the machine-readable outcome of one authorization evaluation.
// Code is the stable signal automation keys on; Reason is human-facing detail
// drawn from a fixed set of authored strings (never caller input), so a
// decision can be logged and rendered without becoming a leak path.
type Decision struct {
	Effect string
	Code   string
	Reason string
	// Mode records which posture produced the decision (see Mode* constants),
	// so an audit record says whether authorization was actually enforced.
	Mode string
	// RuleIndex is the 0-based index of the policy rule that decided, or -1
	// when the default posture decided (no rule matched, or legacy mode).
	RuleIndex int
	// ApprovalID names the approval that satisfied an approval requirement.
	ApprovalID string
}

// Allowed reports whether execution may proceed.
func (d Decision) Allowed() bool { return d.Effect == EffectAllow }

// Err converts a non-allow decision into the structured error the CLI error
// boundary renders and the machine envelope carries. An allow returns nil.
func (d Decision) Err() error {
	if d.Allowed() {
		return nil
	}
	code := phelixerr.Code(d.Code)
	switch code {
	case phelixerr.CodeAuthzDenied, phelixerr.CodeAuthzUnavailable, phelixerr.CodeAuthzInvalid,
		phelixerr.CodeApprovalRequired, phelixerr.CodeApprovalStale, phelixerr.CodeApprovalInvalid:
	default:
		// A decision must never carry an unrecognized code into the error
		// contract; fail closed under the generic denial instead.
		code = phelixerr.CodeAuthzDenied
	}
	return phelixerr.New(code, d.Reason)
}

// Authorizer decides whether a request may execute. Implementations must be
// pure functions of the request and their own configuration: no I/O, no
// clocks, no transport inspection. That is what makes a decision
// deterministic and reproducible from an audit record.
type Authorizer interface {
	Authorize(ctx context.Context, req Request) (Decision, error)
}

// Validate checks that a request is well-formed enough to decide on. An
// invalid request is AUTHZ_INVALID and fails closed — it is never treated as
// an allow, and never as a plain denial that an operator might "fix" by
// writing a rule.
func (r Request) Validate() error {
	if err := r.Actor.Validate(); err != nil {
		return err
	}
	switch r.Action {
	case ActionRebuild, ActionRollback:
	default:
		return invalidf("unknown authorizable action %q", r.Action)
	}
	if r.Target.App == "" {
		return invalidf("authorization request has no target application")
	}
	if r.Plan != nil {
		if r.Plan.ID == "" || r.Plan.Hash == "" {
			return invalidf("plan-bound authorization requires both a plan id and a plan hash")
		}
	}
	return nil
}

// invalidf builds the AUTHZ_INVALID error used for malformed authorization
// input and malformed host policy. It is not retryable: repeating the request
// cannot fix a broken rule set.
func invalidf(format string, args ...any) error {
	return phelixerr.Newf(phelixerr.CodeAuthzInvalid, format, args...)
}
