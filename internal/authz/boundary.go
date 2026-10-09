package authz

import (
	"context"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// boundary.go — the single execution authorization gate.
//
// Every protected mutation entry point calls exactly one function here, and
// the ordering inside it is the Phase 4 security contract:
//
//	authenticate → load policy → resolve approval → decide → audit → execute
//
// Nothing between "decide" and the caller's execution can change the answer,
// because the gate is called at the point mutation is about to begin, not
// minutes earlier (see "Race safety" below). A gate failure is always a
// refusal: there is no path through this file that returns "allowed" because
// something went wrong.

// Outcome is the full, auditable result of passing the boundary. Commands
// normally only need Err(); tests and `authz check` use the rest.
type Outcome struct {
	Decision Decision
	// DecisionID is the audit record's ID. It is set only by [Authorize] —
	// an [Evaluate] (dry-run) records nothing, so it has no ID to report.
	DecisionID string
	Actor      Actor
	// PolicySource is the policy file the decision came from, or "" for the
	// implicit legacy-local policy.
	PolicySource string
}

// Err returns the structured refusal, or nil when execution may proceed.
func (o Outcome) Err() error { return o.Decision.Err() }

// ExecutionRequest is what a mutation entry point knows about itself before
// it mutates anything: which action, on which application, bound to which
// immutable plan (nil for a planless direct invocation).
//
// It carries no transport object, no cobra command, no HTTP request and no
// Phase 2 context — the boundary must stay usable, unchanged, from a future
// API or MCP front end.
type ExecutionRequest struct {
	Action string
	Target Target
	Plan   *PlanRef
}

// Gate is the authorization boundary. Its collaborators are injectable so the
// mutation-safety tests can drive every decision path without a real policy
// on disk, and so a future authentication phase can swap the Authenticator
// without touching a single execution path.
type Gate struct {
	// Authenticator resolves the caller. Defaults to LocalCLIAuthenticator —
	// the only one that exists in Phase 4.
	Authenticator Authenticator
	// LoadPolicy reads the host policy. Defaults to [LoadPolicy].
	LoadPolicy func() (*Policy, error)
	// ResolveApproval resolves a plan's approval state. Defaults to
	// [ResolveApproval].
	ResolveApproval func(planID string) ApprovalState
	// Record persists the decision. Defaults to [RecordDecision].
	Record func(req Request, d Decision, at int64) (string, error)
	// OnRecordError is called when the audit write fails. Defaults to a
	// no-op; the CLI wires it to the log file. Audit failure never changes a
	// decision — it is reported, not obeyed.
	OnRecordError func(error)
}

// DefaultGate is the gate the CLI uses.
func DefaultGate() *Gate { return &Gate{} }

func (g *Gate) authenticator() Authenticator {
	if g != nil && g.Authenticator != nil {
		return g.Authenticator
	}
	return LocalCLIAuthenticator{}
}

func (g *Gate) loadPolicy() (*Policy, error) {
	if g != nil && g.LoadPolicy != nil {
		return g.LoadPolicy()
	}
	return LoadPolicy()
}

func (g *Gate) resolveApproval(planID string) ApprovalState {
	if g != nil && g.ResolveApproval != nil {
		return g.ResolveApproval(planID)
	}
	return ResolveApproval(planID)
}

func (g *Gate) record(req Request, d Decision, at int64) (string, error) {
	if g != nil && g.Record != nil {
		return g.Record(req, d, at)
	}
	return RecordDecision(req, d, at)
}

// Evaluate runs the boundary WITHOUT executing anything and WITHOUT recording
// a decision: it authenticates, loads the policy, resolves the approval and
// decides. It is the one implementation behind both the execution gate and
// `phelix authz check`, so a dry-run answer can never disagree with what an
// apply would do.
//
// Nothing is audited here on purpose. The decision log answers "who was
// allowed or denied EXECUTION of this exact plan", and it is bounded — if a
// read-only query wrote to it, repeated probing could evict real execution
// decisions. [Authorize] is the only path that records.
//
// Every failure mode is a refusal:
//
//	authentication failure  → the error, no decision, no execution
//	policy unreadable       → AUTHZ_UNAVAILABLE (retryable), no execution
//	policy invalid          → AUTHZ_INVALID (not retryable), no execution
//	no matching allow rule  → AUTHZ_DENIED, no execution
//	approval required       → APPROVAL_REQUIRED, no execution
//
// Race safety: the decision describes the policy and approval state at the
// instant it ran. Callers must therefore gate at the point mutation is about
// to begin — which the plan-apply pipeline does, inside the plan-application
// mutex and immediately before the execution seam — rather than authorizing
// early and assuming the answer still holds. A policy or approval change
// between two applies is picked up by the next evaluation; there is
// deliberately no caching and no distributed lock.
func (g *Gate) Evaluate(ctx context.Context, in ExecutionRequest) (Outcome, error) {
	req, d, source, err := g.decide(ctx, in)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Decision: d, Actor: req.Actor, PolicySource: source}, nil
}

// Authorize is the execution gate: it evaluates, records the decision for
// audit, and returns a non-nil error exactly when execution must NOT proceed.
// A caller that ignores the error executes without authorization, so every
// call site returns it immediately.
func (g *Gate) Authorize(ctx context.Context, in ExecutionRequest) (Outcome, error) {
	req, d, source, err := g.decide(ctx, in)
	if err != nil {
		return Outcome{}, err
	}
	out := g.finish(req, d, source)
	return out, out.Err()
}

// decide is the shared evaluation. It never returns an allow decision
// alongside an error, and never an error alongside an allow.
func (g *Gate) decide(ctx context.Context, in ExecutionRequest) (Request, Decision, string, error) {
	actor, err := g.authenticator().Authenticate()
	if err != nil {
		// A failing authenticator is not a denial — it is an inability to
		// establish who is calling, which must fail closed as unavailable.
		return Request{}, Decision{}, "", phelixerr.Wrap(phelixerr.CodeAuthzUnavailable,
			"could not establish the caller's identity", err)
	}

	req := Request{Actor: actor, Action: in.Action, Target: in.Target, Plan: in.Plan}

	policy, policyErr := g.loadPolicy()
	if policyErr != nil {
		// Fail closed with the policy loader's own code (AUTHZ_UNAVAILABLE vs
		// AUTHZ_INVALID), audited like any other refusal so a broken policy is
		// visible in the decision log rather than silently stopping deploys.
		return req, Decision{
			Effect:    EffectDeny,
			Code:      phelixerr.CodeOf(policyErr).String(),
			Reason:    policyErr.Error(),
			Mode:      "unknown",
			RuleIndex: -1,
		}, "", nil
	}

	// The approval is resolved only when there is a plan to bind it to, and
	// only under enforced authorization: the legacy local path must not start
	// reading approval artifacts it never had.
	if in.Plan != nil && policy.Mode == ModeEnforced {
		st := g.resolveApproval(in.Plan.ID)
		req.Approval = &st
	}

	d, err := NewPolicyAuthorizer(policy).Authorize(ctx, req)
	if err != nil {
		return req, Decision{}, "", phelixerr.Wrap(phelixerr.CodeAuthzUnavailable, "evaluate authorization", err)
	}
	return req, d, policy.Source(), nil
}

// finish audits the decision and assembles the outcome.
func (g *Gate) finish(req Request, d Decision, source string) Outcome {
	id, err := g.record(req, d, time.Now().UnixMilli())
	if err != nil && g != nil && g.OnRecordError != nil {
		g.OnRecordError(err)
	}
	return Outcome{Decision: d, DecisionID: id, Actor: req.Actor, PolicySource: source}
}
