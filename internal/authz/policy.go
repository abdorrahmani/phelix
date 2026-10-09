package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/server"
)

// Authorization modes. Phelix has exactly two postures, and the difference
// between them is the only thing an operator has to understand:
//
//	ModeLegacyLocal — the trusted local compatibility path. This is what every
//	  Phelix install has had since Phase 1: a local CLI invocation on this host
//	  executes. It is the posture when NO policy file exists, so introducing
//	  Phase 4 does not break a single existing workflow.
//
//	ModeEnforced — the protected machine execution path. Every deployment
//	  mutation must be bound to an immutable Plan and allowed by a rule.
//	  Nothing executes by default: no matching allow rule means deny.
//
// There is deliberately no third mode, no per-command mode and no "warn only"
// mode: a boundary that can be observed but not enforced is not a boundary.
const (
	ModeLegacyLocal = "legacy_local"
	ModeEnforced    = "enforced"
)

// Rule effects an operator may write. approval_required is NOT a rule effect:
// it is an allow rule carrying RequireApproval, so a rule always says plainly
// whether it permits the action at all.
const (
	ruleEffectAllow = "allow"
	ruleEffectDeny  = "deny"
)

// maxPolicyBytes bounds the policy file. The rule model is deliberately tiny;
// a megabyte of "policy" means something has gone wrong, and an unbounded
// read is a denial-of-service surface on the execution path.
const maxPolicyBytes = 64 * 1024

// maxRules bounds the rule set for the same reason. A host that needs
// hundreds of execution rules needs a real IAM system, which Phase 4
// explicitly is not.
const maxRules = 128

// ActorMatch is the actor half of a rule. Authenticated is a *bool and is
// REQUIRED: a rule that does not say whether it accepts unauthenticated
// callers is rejected as invalid rather than silently granting one. That one
// decision is what keeps "I forgot a field" from becoming "anyone may
// deploy".
//
// ID matches only an actor that HAS an identity, and only an authenticated
// actor ever has one (see [Actor.Validate]), so no unauthenticated caller can
// match an ID-bearing rule however it invokes Phelix.
type ActorMatch struct {
	Type          string `json:"type"`
	ID            string `json:"id,omitempty"`
	Authenticated *bool  `json:"authenticated"`
}

// Rule is one authorization rule: this actor may (or may not) perform this
// action on this target. That is the entire model — there is no expression
// language, no attribute engine, no role hierarchy and no condition syntax,
// because Phase 4's goal is the architectural boundary, not a policy
// programming language.
//
// Wildcards are a bare "*" for a whole field only; there is no globbing, no
// prefix matching and no regular expressions, so a rule cannot accidentally
// match more than it reads as matching.
type Rule struct {
	Actor  ActorMatch `json:"actor"`
	Action string     `json:"action"`
	Target string     `json:"target"`
	Effect string     `json:"effect"`
	// RequireApproval turns an allow into "allowed, once an approval bound to
	// this exact plan exists".
	RequireApproval bool `json:"require_approval,omitempty"`
	// Description is operator documentation. It is never evaluated and never
	// rendered into a decision reason.
	Description string `json:"description,omitempty"`
}

// Policy is the host's execution authorization configuration: the mode and
// the rule set. It lives on the host, under the Phelix data directory —
// never in a repository's phelix.yaml, which an agent editing the project it
// deploys could rewrite to grant itself execution.
type Policy struct {
	SchemaVersion string `json:"schema_version"`
	Mode          string `json:"mode"`
	Rules         []Rule `json:"rules,omitempty"`

	// source records where this policy came from, for `authz status`.
	source string
}

// Source returns the policy file path the policy was loaded from, or "" for
// the implicit legacy-local policy.
func (p *Policy) Source() string { return p.source }

// Dir returns the authorization state directory under the data dir.
func Dir() string { return filepath.Join(server.DataDir(), "authz") }

// PolicyPath returns the host policy file location.
func PolicyPath() string { return filepath.Join(Dir(), "policy.json") }

// legacyPolicy is the implicit policy for a host that has never configured
// authorization. It is returned ONLY for a genuinely absent policy file —
// never for one that exists and could not be read.
func legacyPolicy() *Policy {
	return &Policy{SchemaVersion: SchemaVersion, Mode: ModeLegacyLocal}
}

// LoadPolicy reads the host policy and fails closed on every problem except
// one: the file not existing at all.
//
// The distinction is the whole security posture of Phase 4, so it is worth
// stating precisely:
//
//	absent file            → legacy_local  (explicit, documented default;
//	                         existing local CLI behavior is preserved)
//	present but unreadable → AUTHZ_UNAVAILABLE (retryable, zero mutation)
//	present but invalid    → AUTHZ_INVALID     (not retryable, zero mutation)
//	present and enforced   → the rule set decides; no match means deny
//
// Once a host opts into authorization, a broken policy can therefore never
// degrade into "allow everything" — the failure mode an unconfigured
// production target is most likely to hit.
func LoadPolicy() (*Policy, error) {
	path := PolicyPath()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return legacyPolicy(), nil
	}
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeAuthzUnavailable, "stat authorization policy", err)
	}
	if !info.Mode().IsRegular() {
		return nil, phelixerr.Newf(phelixerr.CodeAuthzInvalid,
			"authorization policy at %s is not a regular file", path)
	}
	if info.Size() > maxPolicyBytes {
		return nil, phelixerr.Newf(phelixerr.CodeAuthzInvalid,
			"authorization policy is %d bytes; the maximum is %d", info.Size(), maxPolicyBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeAuthzUnavailable, "read authorization policy", err)
	}
	p, err := DecodePolicy(data)
	if err != nil {
		return nil, err
	}
	p.source = path
	return p, nil
}

// DecodePolicy parses and fully validates policy bytes. Unknown fields are
// rejected: a typo'd rule key must not silently widen a rule's reach.
func DecodePolicy(data []byte) (*Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeAuthzInvalid,
			"authorization policy is not a parseable policy document", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Policy) validate() error {
	if p.SchemaVersion != SchemaVersion {
		return invalidf("authorization policy schema_version %q, want %q", p.SchemaVersion, SchemaVersion)
	}
	switch p.Mode {
	case ModeLegacyLocal, ModeEnforced:
	default:
		return invalidf("unknown authorization mode %q (want %q or %q)", p.Mode, ModeLegacyLocal, ModeEnforced)
	}
	if len(p.Rules) > maxRules {
		return invalidf("authorization policy has %d rules; the maximum is %d", len(p.Rules), maxRules)
	}
	for i := range p.Rules {
		if err := p.Rules[i].validate(i); err != nil {
			return err
		}
	}
	return nil
}

func (r *Rule) validate(idx int) error {
	switch r.Effect {
	case ruleEffectAllow, ruleEffectDeny:
	default:
		return invalidf("rule %d has unknown effect %q (want %q or %q)", idx, r.Effect, ruleEffectAllow, ruleEffectDeny)
	}
	switch r.Action {
	case ActionRebuild, ActionRollback, "*":
	default:
		return invalidf("rule %d has unknown action %q (want %q, %q or %q)", idx, r.Action, ActionRebuild, ActionRollback, "*")
	}
	if r.Target == "" {
		return invalidf("rule %d has no target (use an application name or %q)", idx, "*")
	}
	switch r.Actor.Type {
	case CallerCLI, CallerAPI, CallerAgent, CallerMCP, CallerService, "*":
	default:
		return invalidf("rule %d has unknown actor type %q", idx, r.Actor.Type)
	}
	if r.Actor.Authenticated == nil {
		return invalidf("rule %d must state actor.authenticated explicitly — "+
			"a rule that does not say whether it accepts unauthenticated callers is never applied", idx)
	}
	if r.Actor.ID != "" && !*r.Actor.Authenticated {
		return invalidf("rule %d names an actor id but accepts unauthenticated callers; "+
			"an unauthenticated caller has no identity to match", idx)
	}
	if r.Effect == ruleEffectDeny && r.RequireApproval {
		return invalidf("rule %d is a deny rule and cannot require approval", idx)
	}
	return nil
}

// matches reports whether the rule applies to this request's actor, action and
// target. Matching is exact-or-wildcard on every field; nothing is inferred.
func (r *Rule) matches(req Request) bool {
	if r.Actor.Type != "*" && r.Actor.Type != req.Actor.Type {
		return false
	}
	if r.Actor.Authenticated == nil || *r.Actor.Authenticated != req.Actor.Authenticated {
		return false
	}
	if r.Actor.ID != "" && r.Actor.ID != "*" {
		// An ID match requires a real, authenticated identity. The Actor
		// invariant (ID implies Authenticated) means a forged ID cannot get
		// here, but the check is explicit so the property is local.
		if !req.Actor.Authenticated || req.Actor.ID != r.Actor.ID {
			return false
		}
	}
	if r.Action != "*" && r.Action != req.Action {
		return false
	}
	if r.Target != "*" && r.Target != req.Target.App {
		return false
	}
	return true
}

// PolicyAuthorizer decides requests against a loaded [Policy]. It performs no
// I/O and consults no clock, so a decision is a pure function of (policy,
// request) — reproducible from an audit record, and trivially testable.
type PolicyAuthorizer struct {
	Policy *Policy
}

// NewPolicyAuthorizer builds an authorizer over a policy.
func NewPolicyAuthorizer(p *Policy) *PolicyAuthorizer { return &PolicyAuthorizer{Policy: p} }

// Authorize evaluates one request. The evaluation order is fixed and total:
//
//  1. a malformed request or policy is AUTHZ_INVALID (fail closed);
//  2. legacy_local mode allows the local CLI path (and nothing else);
//  3. a protected request with no plan binding is denied — under enforced
//     authorization, execution must be bound to an immutable plan;
//  4. an explicit deny rule wins over any allow rule, always;
//  5. an allow rule that requires approval is satisfied only by an approval
//     bound to this exact plan id AND plan hash AND action AND target;
//  6. anything else is denied by default.
func (a *PolicyAuthorizer) Authorize(ctx context.Context, req Request) (Decision, error) {
	if a == nil || a.Policy == nil {
		return denied(phelixerr.CodeAuthzUnavailable.String(),
			"no authorization policy is loaded; refusing to execute", "", -1), nil
	}
	mode := a.Policy.Mode
	if err := req.Validate(); err != nil {
		return Decision{
			Effect:    EffectDeny,
			Code:      phelixerr.CodeOf(err).String(),
			Reason:    err.Error(),
			Mode:      mode,
			RuleIndex: -1,
		}, nil
	}

	// Legacy local compatibility: the documented transition boundary. A host
	// that has not configured authorization behaves exactly as Phase 1-3 did
	// for the local CLI, and for nothing else — an API, agent, MCP or service
	// caller is denied here, because those paths did not exist before Phase 4
	// and must not inherit a trusted-by-default posture.
	if mode == ModeLegacyLocal {
		if req.Actor.Type == CallerCLI {
			return Decision{
				Effect:    EffectAllow,
				Code:      codeAllowed,
				Reason:    "authorization is not enforced on this host (legacy local mode); local CLI execution is permitted",
				Mode:      mode,
				RuleIndex: -1,
			}, nil
		}
		return denied(phelixerr.CodeAuthzDenied.String(),
			"authorization is not enforced on this host, which permits local CLI execution only; "+
				"configure an authorization policy to allow non-CLI callers", mode, -1), nil
	}

	// Enforced: protected execution must be plan-bound. Authorizing a bare
	// action name would let a caller substitute a different plan behind the
	// decision, which is the exact failure Phase 3's immutable plans and this
	// boundary exist to prevent.
	if req.Plan == nil {
		return denied(phelixerr.CodeAuthzDenied.String(),
			"authorization is enforced on this host: protected execution must be bound to an immutable plan — "+
				"create one with 'phelix plan create' and execute it with 'phelix plan apply'", mode, -1), nil
	}

	// Explicit deny is absolute and is evaluated before any allow, so an
	// operator can carve out a target without auditing every allow rule.
	for i := range a.Policy.Rules {
		rule := &a.Policy.Rules[i]
		if rule.Effect == ruleEffectDeny && rule.matches(req) {
			return denied(phelixerr.CodeAuthzDenied.String(),
				"an explicit deny rule forbids this caller from executing this action on this target", mode, i), nil
		}
	}

	for i := range a.Policy.Rules {
		rule := &a.Policy.Rules[i]
		if rule.Effect != ruleEffectAllow || !rule.matches(req) {
			continue
		}
		if !rule.RequireApproval {
			return Decision{
				Effect:    EffectAllow,
				Code:      codeAllowed,
				Reason:    "an allow rule permits this caller to execute this plan",
				Mode:      mode,
				RuleIndex: i,
			}, nil
		}
		return a.decideWithApproval(req, mode, i)
	}

	return denied(phelixerr.CodeAuthzDenied.String(),
		"no authorization rule permits this caller to execute this action on this target", mode, -1), nil
}

// decideWithApproval resolves an approval-requiring allow rule. Every binding
// field must match: an approval is an authorization for one exact plan, not a
// standing permission for an action.
func (a *PolicyAuthorizer) decideWithApproval(req Request, mode string, ruleIdx int) (Decision, error) {
	st := req.Approval
	if st == nil || !st.Present {
		return Decision{
			Effect: EffectApprovalRequired,
			Code:   phelixerr.CodeApprovalRequired.String(),
			Reason: "this execution requires an approval bound to this exact plan; " +
				"approve it with 'phelix authz approve <plan-id>'",
			Mode:      mode,
			RuleIndex: ruleIdx,
		}, nil
	}
	if st.Err != nil {
		return Decision{
			Effect:    EffectDeny,
			Code:      phelixerr.CodeOf(st.Err).String(),
			Reason:    st.Err.Error(),
			Mode:      mode,
			RuleIndex: ruleIdx,
		}, nil
	}
	// The plan ID binding: an approval filed for another plan can never
	// authorize this one, even when the action and target are identical.
	if st.Binding.PlanID != req.Plan.ID {
		return staleApproval(mode, ruleIdx, st.ApprovalID,
			"the approval is bound to a different plan"), nil
	}
	// The plan HASH binding: this is what makes the approval an authorization
	// of specific execution semantics rather than of a plan name. A plan
	// whose content changed no longer matches the approval it was granted.
	if st.Binding.PlanHash != req.Plan.Hash {
		return staleApproval(mode, ruleIdx, st.ApprovalID,
			"the plan's content hash no longer matches the hash that was approved"), nil
	}
	if st.Binding.Action != req.Action || st.Binding.Target != req.Target.App {
		return staleApproval(mode, ruleIdx, st.ApprovalID,
			"the approval's action/target scope does not match this execution"), nil
	}
	return Decision{
		Effect:     EffectAllow,
		Code:       codeAllowed,
		Reason:     "an allow rule permits this caller, and a valid approval is bound to this exact plan",
		Mode:       mode,
		RuleIndex:  ruleIdx,
		ApprovalID: st.ApprovalID,
	}, nil
}

// codeAllowed is the stable decision code for a permitted execution. It is
// not a phelixerr code: an allow is not an error, but automation and audit
// records still need one stable string for it.
const codeAllowed = "AUTHZ_ALLOWED"

func denied(code, reason, mode string, ruleIdx int) Decision {
	return Decision{Effect: EffectDeny, Code: code, Reason: reason, Mode: mode, RuleIndex: ruleIdx}
}

func staleApproval(mode string, ruleIdx int, approvalID, why string) Decision {
	return Decision{
		Effect:     EffectDeny,
		Code:       phelixerr.CodeApprovalStale.String(),
		Reason:     why + "; approvals are never reusable across plans — approve the plan you are applying",
		Mode:       mode,
		RuleIndex:  ruleIdx,
		ApprovalID: approvalID,
	}
}
