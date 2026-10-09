package authz

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// authz_test.go — unit coverage for the authorization boundary's decision
// model, plan binding, approval artifact and audit records.
//
// The command-level proof that a refusal produces ZERO mutation lives in
// cmd/authz_test.go, where a counting execution seam can observe it. These
// tests pin the decisions that gate reaches.

func testDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	return dir
}

func boolPtr(b bool) *bool { return &b }

const (
	testPlanA = "pln_000000000000000a"
	testPlanB = "pln_000000000000000b"
	hashA     = "sha256:aaaa"
	hashB     = "sha256:bbbb"
)

func planRefA() *PlanRef { return &PlanRef{ID: testPlanA, Hash: hashA} }

func cliActor() Actor { return Actor{Type: CallerCLI, Authenticated: false} }

func enforcedPolicy(rules ...Rule) *Policy {
	return &Policy{SchemaVersion: SchemaVersion, Mode: ModeEnforced, Rules: rules}
}

// allowCLIRule permits the unauthenticated local CLI actor to run action on
// target — the shape a host writes to keep local deploys working while
// enforcing the boundary.
func allowCLIRule(action, target string, requireApproval bool) Rule {
	return Rule{
		Actor:           ActorMatch{Type: CallerCLI, Authenticated: boolPtr(false)},
		Action:          action,
		Target:          target,
		Effect:          ruleEffectAllow,
		RequireApproval: requireApproval,
	}
}

func decide(t *testing.T, p *Policy, req Request) Decision {
	t.Helper()
	d, err := NewPolicyAuthorizer(p).Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Authorize returned an error instead of a decision: %v", err)
	}
	return d
}

// --- Actor model -------------------------------------------------------------

func TestActor_UnauthenticatedHasNoIdentity(t *testing.T) {
	actor, err := LocalCLIAuthenticator{}.Authenticate()
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if actor.Type != CallerCLI {
		t.Fatalf("type = %q, want %q", actor.Type, CallerCLI)
	}
	if actor.Authenticated {
		t.Fatal("the local CLI actor must never report itself authenticated")
	}
	if actor.ID != "" {
		t.Fatalf("the local CLI actor must carry no identity, got %q", actor.ID)
	}
	// The Phase 1 wire shape is preserved exactly: id stays null.
	m := actor.Machine()
	if m.ID != nil {
		t.Fatal("machine actor id must stay null for an unauthenticated caller")
	}
	if m.Authenticated {
		t.Fatal("machine actor must report authenticated=false")
	}
}

func TestActor_IdentityWithoutAuthenticationIsInvalid(t *testing.T) {
	// The structural defence against self-identification: an actor claiming an
	// ID without authentication cannot pass validation, so it can never reach
	// a rule match.
	bad := Actor{Type: CallerCLI, ID: "admin", Authenticated: false}
	err := bad.Validate()
	if err == nil {
		t.Fatal("an unauthenticated actor presenting an identity must be rejected")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeAuthzInvalid {
		t.Fatalf("code = %s, want %s", got, phelixerr.CodeAuthzInvalid)
	}
	if bad.String() != "cli:unauthenticated" {
		t.Fatalf("String() = %q, want cli:unauthenticated — a claimed id must not be rendered as identity", bad.String())
	}
}

func TestActor_UnknownCallerTypeIsInvalid(t *testing.T) {
	if err := (Actor{Type: "root", Authenticated: true}).Validate(); err == nil {
		t.Fatal("an unknown caller type must be rejected")
	}
}

// TestAuthorize_NoPrivilegeEscalationBySelfIdentification is Invariant 10 at
// the decision layer: a caller that would like to be an authorized principal
// cannot become one, because the only actor it can present is the
// unauthenticated CLI actor and no amount of claimed metadata changes which
// rules match.
func TestAuthorize_NoPrivilegeEscalationBySelfIdentification(t *testing.T) {
	// The host grants execution to an authenticated service identity only.
	policy := enforcedPolicy(Rule{
		Actor:  ActorMatch{Type: CallerService, ID: "deployer", Authenticated: boolPtr(true)},
		Action: ActionRebuild, Target: "billing", Effect: ruleEffectAllow,
	})

	// A local CLI caller — the only actor Phase 4 can produce — is denied.
	d := decide(t, policy, Request{Actor: cliActor(), Action: ActionRebuild,
		Target: Target{App: "billing"}, Plan: planRefA()})
	if d.Allowed() {
		t.Fatal("the unauthenticated CLI actor must not match an authenticated-service rule")
	}
	if d.Code != phelixerr.CodeAuthzDenied.String() {
		t.Fatalf("code = %q, want AUTHZ_DENIED", d.Code)
	}

	// Even a hand-built actor that claims the exact identity the rule names is
	// refused, because claiming an identity without authentication is invalid
	// input rather than a match.
	forged := Actor{Type: CallerService, ID: "deployer", Authenticated: false}
	d = decide(t, policy, Request{Actor: forged, Action: ActionRebuild,
		Target: Target{App: "billing"}, Plan: planRefA()})
	if d.Allowed() {
		t.Fatal("a forged identity must never be allowed")
	}
	if d.Code != phelixerr.CodeAuthzInvalid.String() {
		t.Fatalf("code = %q, want AUTHZ_INVALID for a forged identity", d.Code)
	}
}

// --- Decision model ----------------------------------------------------------

func TestAuthorize_DecisionMatrix(t *testing.T) {
	approvedA := &ApprovalState{
		Present: true, ApprovalID: "apr_000000000000000a",
		Binding: ApprovalBinding{PlanID: testPlanA, PlanHash: hashA, Action: ActionRebuild, Target: "billing"},
	}

	cases := []struct {
		name       string
		policy     *Policy
		req        Request
		wantEffect string
		wantCode   string
	}{
		{
			name:   "allow rule permits the exact plan",
			policy: enforcedPolicy(allowCLIRule(ActionRebuild, "billing", false)),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: planRefA()},
			wantEffect: EffectAllow, wantCode: codeAllowed,
		},
		{
			name:   "no matching rule denies by default",
			policy: enforcedPolicy(allowCLIRule(ActionRebuild, "other-app", false)),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: planRefA()},
			wantEffect: EffectDeny, wantCode: "AUTHZ_DENIED",
		},
		{
			name:   "empty rule set denies everything",
			policy: enforcedPolicy(),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: planRefA()},
			wantEffect: EffectDeny, wantCode: "AUTHZ_DENIED",
		},
		{
			name: "explicit deny beats an allow rule",
			policy: enforcedPolicy(
				allowCLIRule("*", "*", false),
				Rule{Actor: ActorMatch{Type: "*", Authenticated: boolPtr(false)},
					Action: ActionRebuild, Target: "billing", Effect: ruleEffectDeny},
			),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: planRefA()},
			wantEffect: EffectDeny, wantCode: "AUTHZ_DENIED",
		},
		{
			name:   "wrong action does not match",
			policy: enforcedPolicy(allowCLIRule(ActionRebuild, "billing", false)),
			req: Request{Actor: cliActor(), Action: ActionRollback,
				Target: Target{App: "billing"}, Plan: planRefA()},
			wantEffect: EffectDeny, wantCode: "AUTHZ_DENIED",
		},
		{
			name:   "planless request is denied under enforcement",
			policy: enforcedPolicy(allowCLIRule("*", "*", false)),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: nil},
			wantEffect: EffectDeny, wantCode: "AUTHZ_DENIED",
		},
		{
			name:   "approval-requiring rule without an approval",
			policy: enforcedPolicy(allowCLIRule(ActionRebuild, "billing", true)),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: planRefA()},
			wantEffect: EffectApprovalRequired, wantCode: "APPROVAL_REQUIRED",
		},
		{
			name:   "approval-requiring rule with a matching approval",
			policy: enforcedPolicy(allowCLIRule(ActionRebuild, "billing", true)),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: planRefA(), Approval: approvedA},
			wantEffect: EffectAllow, wantCode: codeAllowed,
		},
		{
			name:   "legacy local mode allows the local CLI path",
			policy: &Policy{SchemaVersion: SchemaVersion, Mode: ModeLegacyLocal},
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: nil},
			wantEffect: EffectAllow, wantCode: codeAllowed,
		},
		{
			name:   "legacy local mode denies a non-CLI caller",
			policy: &Policy{SchemaVersion: SchemaVersion, Mode: ModeLegacyLocal},
			req: Request{Actor: Actor{Type: CallerAgent, Authenticated: false}, Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: planRefA()},
			wantEffect: EffectDeny, wantCode: "AUTHZ_DENIED",
		},
		{
			name:   "unknown action is invalid, not merely denied",
			policy: enforcedPolicy(allowCLIRule("*", "*", false)),
			req: Request{Actor: cliActor(), Action: "delete-everything",
				Target: Target{App: "billing"}, Plan: planRefA()},
			wantEffect: EffectDeny, wantCode: "AUTHZ_INVALID",
		},
		{
			name:   "plan reference without a hash is invalid",
			policy: enforcedPolicy(allowCLIRule("*", "*", false)),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{App: "billing"}, Plan: &PlanRef{ID: testPlanA}},
			wantEffect: EffectDeny, wantCode: "AUTHZ_INVALID",
		},
		{
			name:   "missing target is invalid",
			policy: enforcedPolicy(allowCLIRule("*", "*", false)),
			req: Request{Actor: cliActor(), Action: ActionRebuild,
				Target: Target{}, Plan: planRefA()},
			wantEffect: EffectDeny, wantCode: "AUTHZ_INVALID",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(t, tc.policy, tc.req)
			if d.Effect != tc.wantEffect {
				t.Fatalf("effect = %q, want %q (reason: %s)", d.Effect, tc.wantEffect, d.Reason)
			}
			if d.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", d.Code, tc.wantCode)
			}
			if d.Reason == "" {
				t.Fatal("every decision must carry a reason")
			}
			if !d.Allowed() && d.Err() == nil {
				t.Fatal("a non-allow decision must produce a structured error")
			}
		})
	}
}

func TestAuthorize_IsDeterministic(t *testing.T) {
	policy := enforcedPolicy(allowCLIRule(ActionRebuild, "billing", true))
	req := Request{Actor: cliActor(), Action: ActionRebuild,
		Target: Target{App: "billing"}, Plan: planRefA()}
	first := decide(t, policy, req)
	for i := 0; i < 20; i++ {
		got := decide(t, policy, req)
		if got != first {
			t.Fatalf("decision #%d differed: %+v vs %+v", i, got, first)
		}
	}
}

func TestAuthorize_NilPolicyFailsClosed(t *testing.T) {
	var a *PolicyAuthorizer
	d, err := a.Authorize(context.Background(), Request{Actor: cliActor(),
		Action: ActionRebuild, Target: Target{App: "billing"}, Plan: planRefA()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Allowed() {
		t.Fatal("a missing policy must never allow execution")
	}
	if d.Code != phelixerr.CodeAuthzUnavailable.String() {
		t.Fatalf("code = %q, want AUTHZ_UNAVAILABLE", d.Code)
	}
}

// --- Plan binding ------------------------------------------------------------

// TestAuthorize_ApprovalBindsToExactPlan is Invariants 6, 7 and 8: an
// approval authorizes one plan id + content hash + action + target, and
// nothing else.
func TestAuthorize_ApprovalBindsToExactPlan(t *testing.T) {
	policy := enforcedPolicy(allowCLIRule("*", "*", true))
	approvalForA := ApprovalBinding{PlanID: testPlanA, PlanHash: hashA, Action: ActionRebuild, Target: "billing"}

	cases := []struct {
		name     string
		req      Request
		wantCode string
	}{
		{
			name: "same plan, same hash — allowed",
			req: Request{Actor: cliActor(), Action: ActionRebuild, Target: Target{App: "billing"},
				Plan: &PlanRef{ID: testPlanA, Hash: hashA}},
			wantCode: codeAllowed,
		},
		{
			name: "different plan with the same action and target — refused",
			req: Request{Actor: cliActor(), Action: ActionRebuild, Target: Target{App: "billing"},
				Plan: &PlanRef{ID: testPlanB, Hash: hashA}},
			wantCode: "APPROVAL_STALE",
		},
		{
			name: "same plan id, changed content hash — refused",
			req: Request{Actor: cliActor(), Action: ActionRebuild, Target: Target{App: "billing"},
				Plan: &PlanRef{ID: testPlanA, Hash: hashB}},
			wantCode: "APPROVAL_STALE",
		},
		{
			name: "same plan, different action — refused",
			req: Request{Actor: cliActor(), Action: ActionRollback, Target: Target{App: "billing"},
				Plan: &PlanRef{ID: testPlanA, Hash: hashA}},
			wantCode: "APPROVAL_STALE",
		},
		{
			name: "same plan, different target — refused",
			req: Request{Actor: cliActor(), Action: ActionRebuild, Target: Target{App: "payments"},
				Plan: &PlanRef{ID: testPlanA, Hash: hashA}},
			wantCode: "APPROVAL_STALE",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Approval = &ApprovalState{Present: true, ApprovalID: "apr_000000000000000a", Binding: approvalForA}
			d := decide(t, policy, req)
			if d.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q (reason: %s)", d.Code, tc.wantCode, d.Reason)
			}
			if tc.wantCode != codeAllowed && d.Allowed() {
				t.Fatal("a mismatched approval must never allow execution")
			}
		})
	}
}

func TestAuthorize_BrokenApprovalIsNeverAnApproval(t *testing.T) {
	policy := enforcedPolicy(allowCLIRule("*", "*", true))
	d := decide(t, policy, Request{
		Actor: cliActor(), Action: ActionRebuild, Target: Target{App: "billing"}, Plan: planRefA(),
		Approval: &ApprovalState{Present: true, Err: phelixerr.New(phelixerr.CodeApprovalInvalid, "corrupt")},
	})
	if d.Allowed() {
		t.Fatal("an unverifiable approval must not allow execution")
	}
	if d.Code != phelixerr.CodeApprovalInvalid.String() {
		t.Fatalf("code = %q, want APPROVAL_INVALID", d.Code)
	}
}

// --- Policy loading ---------------------------------------------------------

func TestLoadPolicy_AbsentFileIsLegacyLocal(t *testing.T) {
	testDataDir(t)
	p, err := LoadPolicy()
	if err != nil {
		t.Fatalf("an absent policy must not be an error: %v", err)
	}
	if p.Mode != ModeLegacyLocal {
		t.Fatalf("mode = %q, want %q — existing local workflows must keep working", p.Mode, ModeLegacyLocal)
	}
	if p.Source() != "" {
		t.Fatalf("the implicit policy must report no source, got %q", p.Source())
	}
}

func writePolicyFile(t *testing.T, content string) {
	t.Helper()
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PolicyPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadPolicy_BrokenPolicyFailsClosed is the production-safety invariant:
// once a host has a policy file, no form of brokenness degrades into
// "allow everything".
func TestLoadPolicy_BrokenPolicyFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantCode phelixerr.Code
	}{
		{"unparseable", "{not json", phelixerr.CodeAuthzInvalid},
		{"foreign schema", `{"schema_version":"99","mode":"enforced"}`, phelixerr.CodeAuthzInvalid},
		{"unknown mode", `{"schema_version":"1","mode":"permissive"}`, phelixerr.CodeAuthzInvalid},
		{"unknown field", `{"schema_version":"1","mode":"enforced","allow_all":true}`, phelixerr.CodeAuthzInvalid},
		{
			name: "rule without an explicit authenticated field",
			content: `{"schema_version":"1","mode":"enforced","rules":[` +
				`{"actor":{"type":"*"},"action":"*","target":"*","effect":"allow"}]}`,
			wantCode: phelixerr.CodeAuthzInvalid,
		},
		{
			name: "rule naming an id but accepting unauthenticated callers",
			content: `{"schema_version":"1","mode":"enforced","rules":[` +
				`{"actor":{"type":"cli","id":"admin","authenticated":false},"action":"*","target":"*","effect":"allow"}]}`,
			wantCode: phelixerr.CodeAuthzInvalid,
		},
		{
			name: "unknown effect",
			content: `{"schema_version":"1","mode":"enforced","rules":[` +
				`{"actor":{"type":"*","authenticated":false},"action":"*","target":"*","effect":"maybe"}]}`,
			wantCode: phelixerr.CodeAuthzInvalid,
		},
		{
			name: "unknown action",
			content: `{"schema_version":"1","mode":"enforced","rules":[` +
				`{"actor":{"type":"*","authenticated":false},"action":"drop-database","target":"*","effect":"allow"}]}`,
			wantCode: phelixerr.CodeAuthzInvalid,
		},
		{
			name: "deny rule requiring approval",
			content: `{"schema_version":"1","mode":"enforced","rules":[` +
				`{"actor":{"type":"*","authenticated":false},"action":"*","target":"*","effect":"deny","require_approval":true}]}`,
			wantCode: phelixerr.CodeAuthzInvalid,
		},
		{
			name: "empty target",
			content: `{"schema_version":"1","mode":"enforced","rules":[` +
				`{"actor":{"type":"*","authenticated":false},"action":"*","target":"","effect":"allow"}]}`,
			wantCode: phelixerr.CodeAuthzInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testDataDir(t)
			writePolicyFile(t, tc.content)
			p, err := LoadPolicy()
			if err == nil {
				t.Fatalf("a broken policy must fail closed, got mode %q", p.Mode)
			}
			if got := phelixerr.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %s, want %s (err: %v)", got, tc.wantCode, err)
			}
			if phelixerr.Retryable(phelixerr.CodeOf(err)) {
				t.Fatal("an invalid policy is not fixed by retrying and must not be marked retryable")
			}
		})
	}
}

func TestLoadPolicy_UnreadablePolicyIsUnavailableAndRetryable(t *testing.T) {
	testDataDir(t)
	// A directory where the policy file belongs: readable metadata, but not a
	// policy — the boundary must refuse rather than fall back.
	if err := os.MkdirAll(PolicyPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(); err == nil {
		t.Fatal("a non-regular policy path must fail closed")
	}

	// An I/O failure is distinguishable from invalid content, and retryable.
	if !phelixerr.Retryable(phelixerr.CodeAuthzUnavailable) {
		t.Fatal("AUTHZ_UNAVAILABLE must be retryable: the policy may become readable again")
	}
	if phelixerr.Retryable(phelixerr.CodeAuthzDenied) {
		t.Fatal("AUTHZ_DENIED must never be retryable: an agent must not repeat a denied action")
	}
	if phelixerr.Retryable(phelixerr.CodeApprovalRequired) {
		t.Fatal("APPROVAL_REQUIRED must not be retryable: it needs an approval, not a retry")
	}
}

func TestLoadPolicy_ValidEnforcedPolicy(t *testing.T) {
	testDataDir(t)
	writePolicyFile(t, `{
  "schema_version": "1",
  "mode": "enforced",
  "rules": [
    {"actor": {"type": "cli", "authenticated": false},
     "action": "rebuild", "target": "billing", "effect": "allow",
     "require_approval": true, "description": "local operator, with approval"}
  ]
}`)
	p, err := LoadPolicy()
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.Mode != ModeEnforced || len(p.Rules) != 1 {
		t.Fatalf("policy = %+v", p)
	}
	if p.Source() != PolicyPath() {
		t.Fatalf("source = %q, want %q", p.Source(), PolicyPath())
	}
	if !p.Rules[0].RequireApproval {
		t.Fatal("require_approval was lost")
	}
}

// --- Approval artifact ------------------------------------------------------

func newTestApproval(t *testing.T, planID, planHash, action, target string) *Approval {
	t.Helper()
	a, err := NewApproval(cliActor(), PlanRef{ID: planID, Hash: planHash}, action, target, "tester")
	if err != nil {
		t.Fatalf("NewApproval: %v", err)
	}
	return a
}

func TestApproval_SaveLoadRoundTrip(t *testing.T) {
	testDataDir(t)
	a := newTestApproval(t, testPlanA, hashA, ActionRebuild, "billing")
	if err := SaveApproval(a); err != nil {
		t.Fatalf("SaveApproval: %v", err)
	}
	loaded, err := LoadApproval(testPlanA)
	if err != nil {
		t.Fatalf("LoadApproval: %v", err)
	}
	if loaded == nil {
		t.Fatal("approval not found after saving")
	}
	if loaded.ApprovalID != a.ApprovalID || loaded.PlanHash != hashA || loaded.Decision != DecisionApproved {
		t.Fatalf("round trip lost data: %+v", loaded)
	}
	if loaded.ApproverAuthenticated {
		t.Fatal("a local-host approval must not claim an authenticated approver")
	}
	if loaded.ApproverSource != ApproverSourceLocalHost {
		t.Fatalf("approver_source = %q, want %q", loaded.ApproverSource, ApproverSourceLocalHost)
	}
}

func TestApproval_IsImmutable(t *testing.T) {
	testDataDir(t)
	a := newTestApproval(t, testPlanA, hashA, ActionRebuild, "billing")
	if err := SaveApproval(a); err != nil {
		t.Fatalf("SaveApproval: %v", err)
	}
	// A second approval for the same plan cannot replace the first.
	second := newTestApproval(t, testPlanA, hashA, ActionRebuild, "billing")
	err := SaveApproval(second)
	if err == nil {
		t.Fatal("re-approving a plan must not silently replace the approval")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeAlreadyExists {
		t.Fatalf("code = %s, want ALREADY_EXISTS", got)
	}
	loaded, err := LoadApproval(testPlanA)
	if err != nil || loaded.ApprovalID != a.ApprovalID {
		t.Fatal("the original approval must survive a re-approval attempt")
	}
}

func TestApproval_TamperedArtifactIsRefused(t *testing.T) {
	testDataDir(t)
	a := newTestApproval(t, testPlanA, hashA, ActionRebuild, "billing")
	if err := SaveApproval(a); err != nil {
		t.Fatalf("SaveApproval: %v", err)
	}
	// Re-point the approval at different plan content, keeping its hash: the
	// stored approval_hash no longer covers the content.
	path := filepath.Join(ApprovalsDir(), testPlanA+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), hashA, hashB, 1)
	if tampered == string(raw) {
		t.Fatal("test setup did not modify the artifact")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadApproval(testPlanA); err == nil {
		t.Fatal("a tampered approval must be refused")
	} else if got := phelixerr.CodeOf(err); got != phelixerr.CodeApprovalInvalid {
		t.Fatalf("code = %s, want APPROVAL_INVALID", got)
	}
	st := ResolveApproval(testPlanA)
	if st.Err == nil {
		t.Fatal("ResolveApproval must surface the verification failure")
	}
}

// TestApproval_CopiedToAnotherPlanIsRefused closes the obvious reuse attempt:
// placing plan A's approval where plan B's would be found.
func TestApproval_CopiedToAnotherPlanIsRefused(t *testing.T) {
	testDataDir(t)
	a := newTestApproval(t, testPlanA, hashA, ActionRebuild, "billing")
	if err := SaveApproval(a); err != nil {
		t.Fatalf("SaveApproval: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(ApprovalsDir(), testPlanA+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ApprovalsDir(), testPlanB+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApproval(testPlanB); err == nil {
		t.Fatal("plan A's approval must not be usable as plan B's")
	} else if got := phelixerr.CodeOf(err); got != phelixerr.CodeApprovalInvalid {
		t.Fatalf("code = %s, want APPROVAL_INVALID", got)
	}
}

func TestApproval_MissingIsNotAnError(t *testing.T) {
	testDataDir(t)
	a, err := LoadApproval(testPlanA)
	if err != nil {
		t.Fatalf("a missing approval must not be an error: %v", err)
	}
	if a != nil {
		t.Fatal("expected no approval")
	}
	st := ResolveApproval(testPlanA)
	if st.Present || st.Err != nil {
		t.Fatalf("resolved state = %+v, want absent", st)
	}
}

func TestApproval_RevokeRemovesTheArtifact(t *testing.T) {
	testDataDir(t)
	if err := SaveApproval(newTestApproval(t, testPlanA, hashA, ActionRebuild, "billing")); err != nil {
		t.Fatal(err)
	}
	removed, err := RevokeApproval(testPlanA)
	if err != nil || !removed {
		t.Fatalf("RevokeApproval: removed=%t err=%v", removed, err)
	}
	if st := ResolveApproval(testPlanA); st.Present {
		t.Fatal("a revoked approval must no longer resolve")
	}
	removed, err = RevokeApproval(testPlanA)
	if err != nil || removed {
		t.Fatalf("revoking twice must be a no-op: removed=%t err=%v", removed, err)
	}
}

func TestApproval_RejectsMalformedInput(t *testing.T) {
	testDataDir(t)
	if _, err := NewApproval(cliActor(), PlanRef{ID: testPlanA}, ActionRebuild, "billing", ""); err == nil {
		t.Fatal("an approval without a plan hash must be rejected")
	}
	if _, err := NewApproval(cliActor(), PlanRef{ID: testPlanA, Hash: hashA}, "sudo", "billing", ""); err == nil {
		t.Fatal("an approval for an unknown action must be rejected")
	}
	if _, err := NewApproval(cliActor(), PlanRef{ID: testPlanA, Hash: hashA}, ActionRebuild, "", ""); err == nil {
		t.Fatal("an approval without a target must be rejected")
	}
	// A path-traversing plan id must never reach the filesystem.
	bad := newTestApproval(t, testPlanA, hashA, ActionRebuild, "billing")
	bad.PlanID = "../../etc/passwd"
	if err := SaveApproval(bad); err == nil {
		t.Fatal("a malformed plan id must be rejected before any write")
	}
}

// --- Audit trail -------------------------------------------------------------

func TestRecordDecision_IsTraceableAndSecretFree(t *testing.T) {
	testDataDir(t)
	req := Request{
		Actor:  cliActor(),
		Action: ActionRebuild,
		Target: Target{App: "billing", AppID: "11111111-1111-1111-1111-111111111111"},
		Plan:   planRefA(),
	}
	d := Decision{Effect: EffectAllow, Code: codeAllowed, Reason: "allowed by rule",
		Mode: ModeEnforced, RuleIndex: 0, ApprovalID: "apr_000000000000000a"}

	id, err := RecordDecision(req, d, 1700000000000)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if !strings.HasPrefix(id, decisionIDPrefix) {
		t.Fatalf("decision id %q lacks the %s prefix", id, decisionIDPrefix)
	}

	recs, skipped, err := ListDecisions("", 0)
	if err != nil || skipped != 0 {
		t.Fatalf("ListDecisions: skipped=%d err=%v", skipped, err)
	}
	if len(recs) != 1 {
		t.Fatalf("recorded %d decisions, want 1", len(recs))
	}
	rec := recs[0]
	// Traceability: who, what, which exact plan, what was decided, when.
	if rec.ActorType != CallerCLI || rec.ActorAuthenticated {
		t.Fatalf("actor not recorded honestly: %+v", rec)
	}
	if rec.Action != ActionRebuild || rec.Target != "billing" {
		t.Fatalf("action/target not recorded: %+v", rec)
	}
	if rec.PlanID != testPlanA || rec.PlanHash != hashA {
		t.Fatalf("plan binding not recorded: %+v", rec)
	}
	if rec.Decision != EffectAllow || rec.Code != codeAllowed || rec.DecidedAtMs != 1700000000000 {
		t.Fatalf("decision not recorded: %+v", rec)
	}
	if rec.ApprovalID != "apr_000000000000000a" {
		t.Fatalf("approval correlation not recorded: %+v", rec)
	}
	if rec.OperationID != "" {
		t.Fatal("a decision predates its operation and must not claim an operation id")
	}

	// Structural secret-freedom: the record's JSON has no field that could
	// carry one, and the only free-text field passes through the redactor.
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"password", "token", "secret", "api_key", "authorization", "private_key"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("audit record carries a %q-shaped field: %s", forbidden, raw)
		}
	}
}

func TestRecordDecision_RedactsCredentialShapedReasons(t *testing.T) {
	testDataDir(t)
	// Defence in depth: reasons are authored constants, but the audit path
	// still routes them through the shared redactor rather than trusting that.
	d := Decision{Effect: EffectDeny, Code: phelixerr.CodeAuthzDenied.String(),
		Reason: "denied (token=ghp_abcdefghijklmnopqrstuvwxyz0123456789)", Mode: ModeEnforced, RuleIndex: -1}
	if _, err := RecordDecision(Request{Actor: cliActor(), Action: ActionRebuild,
		Target: Target{App: "billing"}, Plan: planRefA()}, d, 1); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	recs, _, err := ListDecisions("", 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("ListDecisions: %d records, err=%v", len(recs), err)
	}
	if strings.Contains(recs[0].Reason, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("audit reason was not redacted: %q", recs[0].Reason)
	}
}

func TestListDecisions_FiltersByPlanAndStaysBounded(t *testing.T) {
	testDataDir(t)
	for i := 0; i < maxDecisionRecords+20; i++ {
		plan := planRefA()
		if i%2 == 0 {
			plan = &PlanRef{ID: testPlanB, Hash: hashB}
		}
		if _, err := RecordDecision(Request{Actor: cliActor(), Action: ActionRebuild,
			Target: Target{App: "billing"}, Plan: plan},
			Decision{Effect: EffectDeny, Code: "AUTHZ_DENIED", Reason: "r", Mode: ModeEnforced, RuleIndex: -1},
			int64(i)); err != nil {
			t.Fatalf("RecordDecision #%d: %v", i, err)
		}
	}
	all, skipped, err := ListDecisions("", 0)
	if err != nil || skipped != 0 {
		t.Fatalf("ListDecisions: skipped=%d err=%v", skipped, err)
	}
	if len(all) > maxDecisionRecords {
		t.Fatalf("decision log kept %d records, cap is %d", len(all), maxDecisionRecords)
	}
	// Newest first.
	if len(all) > 1 && all[0].DecidedAtMs < all[1].DecidedAtMs {
		t.Fatal("decisions must be listed newest first")
	}
	forA, _, err := ListDecisions(testPlanA, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range forA {
		if rec.PlanID != testPlanA {
			t.Fatalf("plan filter leaked plan %s", rec.PlanID)
		}
	}
}

func TestListDecisions_SkipsCorruptLines(t *testing.T) {
	testDataDir(t)
	if _, err := RecordDecision(Request{Actor: cliActor(), Action: ActionRebuild,
		Target: Target{App: "billing"}, Plan: planRefA()},
		Decision{Effect: EffectAllow, Code: codeAllowed, Reason: "r", Mode: ModeEnforced}, 1); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(DecisionsPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	recs, skipped, err := ListDecisions("", 0)
	if err != nil {
		t.Fatalf("a damaged record must not fail the query: %v", err)
	}
	if skipped != 1 || len(recs) != 1 {
		t.Fatalf("recs=%d skipped=%d, want 1 and 1", len(recs), skipped)
	}
}
