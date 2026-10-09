package authz

import (
	"context"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// boundary_test.go — the gate's composition: authenticate → policy → approval
// → decide → audit. Every failure path must refuse; none may return "allowed".

// stubAuthenticator lets a test present a caller a future authentication
// phase would produce, without Phase 4 pretending it can produce one.
type stubAuthenticator struct {
	actor Actor
	err   error
}

func (s stubAuthenticator) Authenticate() (Actor, error) { return s.actor, s.err }

// testGate builds a gate with everything injected, so no disk state is
// involved and the audit log is observable in memory.
func testGate(policy *Policy, policyErr error, approval ApprovalState) (*Gate, *[]DecisionRecord) {
	var recorded []DecisionRecord
	g := &Gate{
		Authenticator:   LocalCLIAuthenticator{},
		LoadPolicy:      func() (*Policy, error) { return policy, policyErr },
		ResolveApproval: func(string) ApprovalState { return approval },
		Record: func(req Request, d Decision, at int64) (string, error) {
			rec := DecisionRecord{
				DecisionID: "azd_000000000000000f", DecidedAtMs: at,
				ActorType: req.Actor.Type, ActorAuthenticated: req.Actor.Authenticated,
				Action: req.Action, Target: req.Target.App,
				Decision: d.Effect, Code: d.Code, Mode: d.Mode, RuleIndex: d.RuleIndex,
				ApprovalID: d.ApprovalID,
			}
			if req.Plan != nil {
				rec.PlanID, rec.PlanHash = req.Plan.ID, req.Plan.Hash
			}
			recorded = append(recorded, rec)
			return rec.DecisionID, nil
		},
	}
	return g, &recorded
}

func planExecRequest() ExecutionRequest {
	return ExecutionRequest{
		Action: ActionRebuild,
		Target: Target{App: "billing"},
		Plan:   &PlanRef{ID: testPlanA, Hash: hashA},
	}
}

func TestGate_AllowsAndAudits(t *testing.T) {
	g, recorded := testGate(enforcedPolicy(allowCLIRule(ActionRebuild, "billing", false)), nil, ApprovalState{})
	out, err := g.Authorize(context.Background(), planExecRequest())
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !out.Decision.Allowed() {
		t.Fatalf("decision = %+v, want allow", out.Decision)
	}
	if out.DecisionID == "" {
		t.Fatal("an allowed execution must carry a decision id for correlation")
	}
	if len(*recorded) != 1 {
		t.Fatalf("recorded %d decisions, want 1", len(*recorded))
	}
	rec := (*recorded)[0]
	if rec.PlanID != testPlanA || rec.PlanHash != hashA || rec.Decision != EffectAllow {
		t.Fatalf("audit record lost the binding: %+v", rec)
	}
}

func TestGate_RefusesOnEveryFailurePath(t *testing.T) {
	cases := []struct {
		name      string
		policy    *Policy
		policyErr error
		approval  ApprovalState
		req       ExecutionRequest
		wantCode  phelixerr.Code
	}{
		{
			name:     "no matching rule",
			policy:   enforcedPolicy(),
			req:      planExecRequest(),
			wantCode: phelixerr.CodeAuthzDenied,
		},
		{
			name:     "approval required",
			policy:   enforcedPolicy(allowCLIRule("*", "*", true)),
			req:      planExecRequest(),
			wantCode: phelixerr.CodeApprovalRequired,
		},
		{
			name:   "stale approval",
			policy: enforcedPolicy(allowCLIRule("*", "*", true)),
			approval: ApprovalState{Present: true, ApprovalID: "apr_000000000000000a",
				Binding: ApprovalBinding{PlanID: testPlanB, PlanHash: hashB, Action: ActionRebuild, Target: "billing"}},
			req:      planExecRequest(),
			wantCode: phelixerr.CodeApprovalStale,
		},
		{
			name:     "invalid approval artifact",
			policy:   enforcedPolicy(allowCLIRule("*", "*", true)),
			approval: ApprovalState{Present: true, Err: phelixerr.New(phelixerr.CodeApprovalInvalid, "corrupt approval")},
			req:      planExecRequest(),
			wantCode: phelixerr.CodeApprovalInvalid,
		},
		{
			name:      "policy unreadable",
			policyErr: phelixerr.New(phelixerr.CodeAuthzUnavailable, "io error"),
			req:       planExecRequest(),
			wantCode:  phelixerr.CodeAuthzUnavailable,
		},
		{
			name:      "policy invalid",
			policyErr: phelixerr.New(phelixerr.CodeAuthzInvalid, "bad rule"),
			req:       planExecRequest(),
			wantCode:  phelixerr.CodeAuthzInvalid,
		},
		{
			name:     "planless under enforcement",
			policy:   enforcedPolicy(allowCLIRule("*", "*", false)),
			req:      ExecutionRequest{Action: ActionRebuild, Target: Target{App: "billing"}},
			wantCode: phelixerr.CodeAuthzDenied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, recorded := testGate(tc.policy, tc.policyErr, tc.approval)
			out, err := g.Authorize(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("the gate must refuse; decision was %+v", out.Decision)
			}
			if got := phelixerr.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %s, want %s (err: %v)", got, tc.wantCode, err)
			}
			if out.Decision.Allowed() {
				t.Fatal("a refusal must never carry an allow decision")
			}
			// Every refusal is audited: a denial that leaves no trace is not
			// an auditable boundary.
			if len(*recorded) != 1 {
				t.Fatalf("recorded %d decisions, want 1", len(*recorded))
			}
		})
	}
}

func TestGate_AuthenticationFailureFailsClosed(t *testing.T) {
	g, recorded := testGate(enforcedPolicy(allowCLIRule("*", "*", false)), nil, ApprovalState{})
	g.Authenticator = stubAuthenticator{err: phelixerr.New(phelixerr.CodeUnavailable, "credential store down")}

	out, err := g.Authorize(context.Background(), planExecRequest())
	if err == nil {
		t.Fatal("an unresolvable caller must not be authorized")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeAuthzUnavailable {
		t.Fatalf("code = %s, want AUTHZ_UNAVAILABLE", got)
	}
	if out.Decision.Allowed() {
		t.Fatal("no decision may be an allow when the caller is unknown")
	}
	if len(*recorded) != 0 {
		t.Fatal("there is no decision to audit when the caller could not be established")
	}
}

// TestGate_FutureCallerTypesPlugInWithoutRuleChanges is the forward-looking
// property: an authenticated non-CLI caller (an API, agent, MCP or service
// identity a later phase introduces) is authorized by the SAME rules through
// the SAME gate. Only the Authenticator changes.
func TestGate_FutureCallerTypesPlugInWithoutRuleChanges(t *testing.T) {
	policy := enforcedPolicy(Rule{
		Actor:  ActorMatch{Type: CallerMCP, ID: "mcp-1", Authenticated: boolPtr(true)},
		Action: ActionRebuild, Target: "billing", Effect: ruleEffectAllow,
	})

	// The Phase 4 CLI caller is denied by this rule...
	g, _ := testGate(policy, nil, ApprovalState{})
	if _, err := g.Authorize(context.Background(), planExecRequest()); err == nil {
		t.Fatal("the local CLI actor must not match an MCP rule")
	}

	// ...and a future authenticated MCP caller is allowed, with no change to
	// the policy, the decision logic or the execution path.
	g, _ = testGate(policy, nil, ApprovalState{})
	g.Authenticator = stubAuthenticator{actor: Actor{Type: CallerMCP, ID: "mcp-1", Authenticated: true}}
	out, err := g.Authorize(context.Background(), planExecRequest())
	if err != nil {
		t.Fatalf("an authenticated MCP caller matching a rule must be allowed: %v", err)
	}
	if !out.Decision.Allowed() {
		t.Fatalf("decision = %+v, want allow", out.Decision)
	}
}

// TestGate_EvaluateMatchesAuthorize pins that `authz check` cannot disagree
// with what an apply would do: both run the same evaluation. It also pins
// that only Authorize records — a read-only query must not be able to evict
// execution decisions from the bounded audit log.
func TestGate_EvaluateMatchesAuthorize(t *testing.T) {
	for _, requireApproval := range []bool{false, true} {
		policy := enforcedPolicy(allowCLIRule("*", "*", requireApproval))
		g1, evalRecorded := testGate(policy, nil, ApprovalState{})
		g2, authRecorded := testGate(policy, nil, ApprovalState{})

		evaluated, err := g1.Evaluate(context.Background(), planExecRequest())
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		authorized, authErr := g2.Authorize(context.Background(), planExecRequest())
		if evaluated.Decision != authorized.Decision {
			t.Fatalf("check and apply disagree: %+v vs %+v", evaluated.Decision, authorized.Decision)
		}
		if (authErr == nil) != evaluated.Decision.Allowed() {
			t.Fatalf("Authorize error (%v) disagrees with the decision effect %q", authErr, evaluated.Decision.Effect)
		}
		if len(*evalRecorded) != 0 {
			t.Fatalf("Evaluate recorded %d decision(s); a dry run must not write to the audit log", len(*evalRecorded))
		}
		if evaluated.DecisionID != "" {
			t.Fatal("a dry run has no audit record and must not report a decision id")
		}
		if len(*authRecorded) != 1 {
			t.Fatalf("Authorize recorded %d decision(s), want 1", len(*authRecorded))
		}
	}
}

// TestGate_LegacyModeDoesNotConsultApprovals pins that introducing Phase 4
// does not make an unconfigured host start depending on approval artifacts.
func TestGate_LegacyModeDoesNotConsultApprovals(t *testing.T) {
	resolved := 0
	g := &Gate{
		LoadPolicy: func() (*Policy, error) {
			return &Policy{SchemaVersion: SchemaVersion, Mode: ModeLegacyLocal}, nil
		},
		ResolveApproval: func(string) ApprovalState { resolved++; return ApprovalState{} },
		Record:          func(Request, Decision, int64) (string, error) { return "azd_0", nil },
	}
	if _, err := g.Authorize(context.Background(), planExecRequest()); err != nil {
		t.Fatalf("legacy local mode must keep permitting local CLI execution: %v", err)
	}
	if resolved != 0 {
		t.Fatalf("legacy mode consulted the approval store %d time(s); it must not", resolved)
	}
}

// TestGate_AuditFailureDoesNotChangeTheDecision: the audit log is a record,
// not the gate. A write failure must neither block an authorized execution
// nor rescue a denied one.
func TestGate_AuditFailureDoesNotChangeTheDecision(t *testing.T) {
	var reported int
	newGate := func(policy *Policy) *Gate {
		return &Gate{
			LoadPolicy:      func() (*Policy, error) { return policy, nil },
			ResolveApproval: func(string) ApprovalState { return ApprovalState{} },
			Record: func(Request, Decision, int64) (string, error) {
				return "", phelixerr.New(phelixerr.CodeFilesystem, "disk full")
			},
			OnRecordError: func(error) { reported++ },
		}
	}

	if _, err := newGate(enforcedPolicy(allowCLIRule("*", "*", false))).
		Authorize(context.Background(), planExecRequest()); err != nil {
		t.Fatalf("an allowed execution must survive an audit-write failure: %v", err)
	}
	if _, err := newGate(enforcedPolicy()).
		Authorize(context.Background(), planExecRequest()); err == nil {
		t.Fatal("a denied execution must stay denied when the audit write fails")
	}
	if reported != 2 {
		t.Fatalf("audit failures reported %d time(s), want 2", reported)
	}
}
