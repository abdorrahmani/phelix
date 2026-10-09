package cmd

// authz_test.go — Phase 4 command-level security contract.
//
// The central suite is TestPlanApply_AuthorizationMutationSafety: for EVERY
// way the authorization boundary can refuse, the execution seam must be
// invoked ZERO times. These tests prove that no mutation happens, not merely
// that an error came back.
//
// Policies are written as real files in an isolated PHELIX_DATA_DIR and read
// through the production loader, so the tests exercise the same code path a
// host does — a policy that is valid here is valid in production.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/abdorrahmani/phelix/internal/authz"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/plans"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// --- policy fixtures ---------------------------------------------------------

// writeAuthzPolicy installs a host policy in the isolated data dir.
func writeAuthzPolicy(t *testing.T, content string) {
	t.Helper()
	if err := os.MkdirAll(authz.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authz.PolicyPath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// enforcedNoRules is the strictest posture: authorization on, nothing allowed.
const enforcedNoRules = `{"schema_version":"1","mode":"enforced","rules":[]}`

// enforcedWithApproval allows the local CLI actor to rebuild anything, but
// only with an approval bound to the exact plan.
const enforcedWithApproval = `{
  "schema_version": "1",
  "mode": "enforced",
  "rules": [
    {"actor": {"type": "cli", "authenticated": false},
     "action": "rebuild", "target": "*", "effect": "allow", "require_approval": true}
  ]
}`

// enforcedNoApproval allows the local CLI actor to rebuild without approval.
const enforcedNoApproval = `{
  "schema_version": "1",
  "mode": "enforced",
  "rules": [
    {"actor": {"type": "cli", "authenticated": false},
     "action": "rebuild", "target": "*", "effect": "allow"}
  ]
}`

// approvePlanForTest records an approval through the same API the
// `phelix authz approve` command uses — never by hand-writing the artifact.
func approvePlanForTest(t *testing.T, plan *plans.Plan) *authz.Approval {
	t.Helper()
	return approveBindingForTest(t, plan.PlanID, plan.PlanHash, plan.Action.Type, plan.Action.Application)
}

func approveBindingForTest(t *testing.T, planID, planHash, action, target string) *authz.Approval {
	t.Helper()
	approver, err := authz.LocalCLIAuthenticator{}.Authenticate()
	if err != nil {
		t.Fatal(err)
	}
	a, err := authz.NewApproval(approver, authz.PlanRef{ID: planID, Hash: planHash}, action, target, "test")
	if err != nil {
		t.Fatalf("NewApproval: %v", err)
	}
	if err := authz.SaveApproval(a); err != nil {
		t.Fatalf("SaveApproval: %v", err)
	}
	return a
}

// --- Invariants 1-5: every refusal is a zero-mutation outcome ---------------

func TestPlanApply_AuthorizationMutationSafety(t *testing.T) {
	cases := []struct {
		name string
		// setup installs the policy and any approval state, after the plan
		// exists. It may return a plan ID to apply instead of the created one.
		setup    func(t *testing.T, plan *plans.Plan)
		wantCode phelixerr.Code
	}{
		{
			name: "authorization denied (no rule permits this execution)",
			setup: func(t *testing.T, plan *plans.Plan) {
				writeAuthzPolicy(t, enforcedNoRules)
			},
			wantCode: phelixerr.CodeAuthzDenied,
		},
		{
			name: "authorization denied (an explicit deny rule)",
			setup: func(t *testing.T, plan *plans.Plan) {
				writeAuthzPolicy(t, fmt.Sprintf(`{"schema_version":"1","mode":"enforced","rules":[
					{"actor":{"type":"cli","authenticated":false},"action":"*","target":"*","effect":"allow"},
					{"actor":{"type":"*","authenticated":false},"action":"rebuild","target":%q,"effect":"deny"}]}`,
					plan.Action.Application))
			},
			wantCode: phelixerr.CodeAuthzDenied,
		},
		{
			name: "approval required and missing",
			setup: func(t *testing.T, plan *plans.Plan) {
				writeAuthzPolicy(t, enforcedWithApproval)
			},
			wantCode: phelixerr.CodeApprovalRequired,
		},
		{
			name: "approval bound to a different plan hash (stale)",
			setup: func(t *testing.T, plan *plans.Plan) {
				writeAuthzPolicy(t, enforcedWithApproval)
				// An approval for this plan ID but for content that is no
				// longer what the plan says it will do.
				approveBindingForTest(t, plan.PlanID, "sha256:0000000000000000",
					plan.Action.Type, plan.Action.Application)
			},
			wantCode: phelixerr.CodeApprovalStale,
		},
		{
			name: "approval bound to a different action (stale)",
			setup: func(t *testing.T, plan *plans.Plan) {
				writeAuthzPolicy(t, enforcedWithApproval)
				approveBindingForTest(t, plan.PlanID, plan.PlanHash,
					plans.ActionRollback, plan.Action.Application)
			},
			wantCode: phelixerr.CodeApprovalStale,
		},
		{
			name: "approval artifact tampered with (invalid)",
			setup: func(t *testing.T, plan *plans.Plan) {
				writeAuthzPolicy(t, enforcedWithApproval)
				approvePlanForTest(t, plan)
				path := filepath.Join(authz.ApprovalsDir(), plan.PlanID+".json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// Widen the approval's scope by hand; the content hash no
				// longer covers it.
				tampered := strings.Replace(string(raw),
					`"target": "`+plan.Action.Application+`"`, `"target": "*"`, 1)
				if tampered == string(raw) {
					t.Fatalf("test setup did not modify the artifact:\n%s", raw)
				}
				if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: phelixerr.CodeApprovalInvalid,
		},
		{
			name: "host policy is invalid",
			setup: func(t *testing.T, plan *plans.Plan) {
				writeAuthzPolicy(t, `{"schema_version":"1","mode":"enforced","rules":[
					{"actor":{"type":"cli"},"action":"*","target":"*","effect":"allow"}]}`)
			},
			wantCode: phelixerr.CodeAuthzInvalid,
		},
		{
			name: "host policy is unreadable",
			setup: func(t *testing.T, plan *plans.Plan) {
				// A directory where the policy belongs: the boundary cannot
				// decide, so it must refuse rather than fall back to legacy.
				if err := os.MkdirAll(authz.PolicyPath(), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: phelixerr.CodeAuthzInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planTestDataDir(t)
			info := withAppStore(t)
			plan := manualRebuildPlan(t, info.Name, info.ID, nil)
			tc.setup(t, plan)

			count := withExecutionSeam(t)
			err := applyPlan(plan.PlanID)
			if err == nil {
				t.Fatal("an unauthorized plan must not apply")
			}
			if got := phelixerr.CodeOf(err); got != tc.wantCode {
				t.Fatalf("code = %s, want %s (err: %v)", got, tc.wantCode, err)
			}
			if n := count.Load(); n != 0 {
				t.Fatalf("MUTATION HAPPENED: execution seam invoked %d time(s) on an unauthorized plan", n)
			}
			if reloaded, loadErr := plans.Load(plan.PlanID); loadErr == nil && reloaded.Status == plans.StatusApplied {
				t.Fatal("an unauthorized plan must never be marked applied")
			}
			// No operation record may exist: authorization precedes the record.
			if recs, _, listErr := ops.List("", 0); listErr == nil && len(recs) != 0 {
				t.Fatalf("a refused execution left %d operation record(s) behind", len(recs))
			}
			if got := ExitCodeFor(err); got != ExitPermission {
				t.Fatalf("exit = %d, want %d (the authorization boundary refused)", got, ExitPermission)
			}
			// The refusal is auditable, and traceable to the exact plan.
			decisions, _, decErr := authz.ListDecisions(plan.PlanID, 0)
			if decErr != nil {
				t.Fatalf("ListDecisions: %v", decErr)
			}
			if len(decisions) == 0 {
				t.Fatal("a refusal must be recorded in the decision log")
			}
			d := decisions[0]
			if d.Decision == authz.EffectAllow {
				t.Fatalf("the recorded decision claims an allow: %+v", d)
			}
			if d.PlanID != plan.PlanID || d.PlanHash != plan.PlanHash {
				t.Fatalf("decision record lost the plan binding: %+v", d)
			}
			if d.OperationID != "" {
				t.Fatal("a refused decision must not reference an operation")
			}
		})
	}
}

// TestPlanApply_ApprovedPlanExecutesAndCorrelates is the positive path, and
// the proof that the whole chain survives: actor → decision → approval →
// plan → operation → deployment.
func TestPlanApply_ApprovedPlanExecutesAndCorrelates(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	writeAuthzPolicy(t, enforcedWithApproval)
	approval := approvePlanForTest(t, plan)

	count := withExecutionSeam(t)
	if err := applyPlan(plan.PlanID); err != nil {
		t.Fatalf("an approved plan must apply: %v", err)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1", n)
	}

	loaded, err := plans.Load(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != plans.StatusApplied || loaded.Correlation == nil {
		t.Fatalf("applied plan lost its correlation: %+v", loaded.Correlation)
	}

	rec, err := ops.Load(loaded.Correlation.OperationID)
	if err != nil {
		t.Fatalf("ops.Load: %v", err)
	}
	if rec.PlanID != plan.PlanID || rec.PlanHash != plan.PlanHash {
		t.Fatalf("operation record lost the plan binding: %+v", rec)
	}
	if rec.ApprovalID != approval.ApprovalID {
		t.Fatalf("operation approval correlation = %q, want %q", rec.ApprovalID, approval.ApprovalID)
	}
	if rec.AuthzDecisionID == "" {
		t.Fatal("an authorized operation must record the decision that permitted it")
	}
	if rec.Actor == nil || rec.Actor.Authenticated || rec.Actor.ID != nil {
		t.Fatalf("operation actor must stay the honest unauthenticated CLI actor: %+v", rec.Actor)
	}

	// The decision log joins to the operation through the decision ID.
	decisions, _, err := authz.ListDecisions(plan.PlanID, 0)
	if err != nil || len(decisions) == 0 {
		t.Fatalf("ListDecisions: %d records, err=%v", len(decisions), err)
	}
	allow := decisions[0]
	if allow.DecisionID != rec.AuthzDecisionID {
		t.Fatalf("decision id mismatch: log %q, operation %q", allow.DecisionID, rec.AuthzDecisionID)
	}
	if allow.Decision != authz.EffectAllow || allow.ApprovalID != approval.ApprovalID {
		t.Fatalf("recorded allow decision is wrong: %+v", allow)
	}
	if allow.Mode != authz.ModeEnforced {
		t.Fatalf("decision mode = %q, want %q", allow.Mode, authz.ModeEnforced)
	}
}

// TestPlanApply_AllowRuleWithoutApprovalExecutes covers the rule-only posture:
// an enforced host that does not require per-plan approval.
func TestPlanApply_AllowRuleWithoutApprovalExecutes(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	writeAuthzPolicy(t, enforcedNoApproval)

	count := withExecutionSeam(t)
	if err := applyPlan(plan.PlanID); err != nil {
		t.Fatalf("an allowed plan must apply: %v", err)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1", n)
	}
	rec, _, err := ops.List("", 0)
	if err != nil || len(rec) != 1 {
		t.Fatalf("ops.List: %d records, err=%v", len(rec), err)
	}
	if rec[0].ApprovalID != "" {
		t.Fatal("no approval was required, so none may be correlated")
	}
	if rec[0].AuthzDecisionID == "" {
		t.Fatal("a rule-authorized operation still records its decision")
	}
}

// --- Invariants 6, 7, 8: a grant for plan A never covers plan B -------------

// TestPlanApply_ApprovalIsNotTransferableBetweenPlans covers both halves:
// an approval for plan A does not authorize plan B even when the action and
// target are identical, and physically relocating the artifact does not help.
func TestPlanApply_ApprovalIsNotTransferableBetweenPlans(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	writeAuthzPolicy(t, enforcedWithApproval)

	planA := manualRebuildPlan(t, info.Name, info.ID, nil)
	// Plan B: same action, same target, same shape — only a different
	// execution input, so it is a genuinely different execution that an
	// action-level grant would wrongly cover.
	planB := manualRebuildPlanLike(t, planA, "variant-b")

	approvePlanForTest(t, planA)
	count := withExecutionSeam(t)

	// Plan A's approval does not reach plan B.
	err := applyPlan(planB.PlanID)
	if err == nil {
		t.Fatal("plan B must not execute under plan A's approval")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeApprovalRequired {
		t.Fatalf("code = %s, want APPROVAL_REQUIRED", got)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: seam invoked %d time(s) for an unapproved plan", n)
	}

	// Neither does copying the artifact to plan B's location.
	raw, err := os.ReadFile(filepath.Join(authz.ApprovalsDir(), planA.PlanID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authz.ApprovalsDir(), planB.PlanID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err = applyPlan(planB.PlanID)
	if err == nil {
		t.Fatal("a relocated approval must not authorize another plan")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeApprovalInvalid {
		t.Fatalf("code = %s, want APPROVAL_INVALID", got)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: seam invoked %d time(s) on a relocated approval", n)
	}

	// Plan A itself still applies: nothing above damaged the valid grant.
	if err := applyPlan(planA.PlanID); err != nil {
		t.Fatalf("plan A must still apply under its own approval: %v", err)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1", n)
	}
}

// TestPlanApply_RevokedApprovalStopsFurtherExecution pins the race-safety
// property: the decision is made at the moment mutation begins, so a
// revocation between two applies is seen by the second one.
func TestPlanApply_RevokedApprovalStopsFurtherExecution(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	writeAuthzPolicy(t, enforcedWithApproval)
	planA := manualRebuildPlan(t, info.Name, info.ID, nil)
	planB := manualRebuildPlanLike(t, planA, "variant-b")

	approvePlanForTest(t, planA)
	approvePlanForTest(t, planB)
	count := withExecutionSeam(t)

	if err := applyPlan(planA.PlanID); err != nil {
		t.Fatalf("applyPlan A: %v", err)
	}
	if removed, err := authz.RevokeApproval(planB.PlanID); err != nil || !removed {
		t.Fatalf("RevokeApproval: removed=%t err=%v", removed, err)
	}
	err := applyPlan(planB.PlanID)
	if err == nil {
		t.Fatal("a revoked approval must stop the next execution")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeApprovalRequired {
		t.Fatalf("code = %s, want APPROVAL_REQUIRED", got)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1 (plan A only)", n)
	}
}

// manualRebuildPlanLike builds a second, distinct plan for the same app: the
// same action and target, a different build tag, and therefore a different
// plan id and plan hash. The tag is chosen because it changes the plan's
// semantic content without changing what the shared resolver re-derives, so
// the plan stays applicable and the test reaches the authorization boundary
// rather than stopping at the Phase 3 drift check.
func manualRebuildPlanLike(t *testing.T, like *plans.Plan, tag string) *plans.Plan {
	t.Helper()
	p := &plans.Plan{
		Status: plans.StatusCreated,
		Action: like.Action,
		Target: like.Target,
		Inputs: plans.Inputs{
			SourceDir:         like.Inputs.SourceDir,
			Port:              like.Inputs.Port,
			Tag:               tag,
			ConfigFingerprint: like.Inputs.ConfigFingerprint,
		},
		Execution:     like.Execution,
		Preconditions: like.Preconditions,
		Capabilities:  like.Capabilities,
	}
	if err := p.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := plans.Save(p); err != nil {
		t.Fatal(err)
	}
	if p.PlanHash == like.PlanHash {
		t.Fatal("test setup produced two identical plans")
	}
	return p
}

// --- Fail-closed does not poison the plan -----------------------------------

// TestPlanApply_RefusalConsumesNoIdempotencyKey is why the gate sits before
// the request-key ledger: a plan refused for want of an approval must apply
// cleanly once it is approved, with no "new plan required" dead end.
func TestPlanApply_RefusalConsumesNoIdempotencyKey(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	writeAuthzPolicy(t, enforcedWithApproval)

	count := withExecutionSeam(t)

	// Three refusals.
	for i := 0; i < 3; i++ {
		err := applyPlan(plan.PlanID)
		if got := phelixerr.CodeOf(err); got != phelixerr.CodeApprovalRequired {
			t.Fatalf("attempt %d: code = %s, want APPROVAL_REQUIRED", i+1, got)
		}
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: seam invoked %d time(s) across refusals", n)
	}

	// Then an approval, and the SAME plan applies.
	approvePlanForTest(t, plan)
	if err := applyPlan(plan.PlanID); err != nil {
		t.Fatalf("the approved plan must apply after earlier refusals: %v", err)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1", n)
	}
}

// --- Phase 3 guarantees survive Phase 4 -------------------------------------

// TestPlanApply_StalenessStillWinsOverAuthorization: a valid authorization
// never resurrects a stale plan, and the plan checks still run first.
func TestPlanApply_StalenessStillWinsOverAuthorization(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, []plans.Precondition{
		{Type: plans.PreconditionCurrentVersion, Expected: "v5", Source: "versions.json"},
	})
	writeAuthzPolicy(t, enforcedWithApproval)
	approvePlanForTest(t, plan)

	count := withExecutionSeam(t)
	err := applyPlan(plan.PlanID)
	if err == nil {
		t.Fatal("an approved but stale plan must not apply")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodePlanStale {
		t.Fatalf("code = %s, want PLAN_STALE — plan validity is independent of authorization", got)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: seam invoked %d time(s) on a stale plan", n)
	}
	if got := ExitCodeFor(err); got != ExitValidation {
		t.Fatalf("exit = %d, want %d — staleness keeps its own exit code", got, ExitValidation)
	}
}

// TestPlanApply_RepeatAfterSuccessIsUnchangedByAuthorization: Phase 3's
// repeat-apply contract must not change meaning because a policy or approval
// was edited after the execution that already happened.
func TestPlanApply_RepeatAfterSuccessIsUnchangedByAuthorization(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	writeAuthzPolicy(t, enforcedWithApproval)
	approvePlanForTest(t, plan)

	count := withExecutionSeam(t)
	if err := applyPlan(plan.PlanID); err != nil {
		t.Fatalf("applyPlan: %v", err)
	}
	applied, err := plans.Load(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	firstOp := applied.Correlation.OperationID

	// Revoke the approval and lock the host down completely.
	if _, err := authz.RevokeApproval(plan.PlanID); err != nil {
		t.Fatal(err)
	}
	writeAuthzPolicy(t, enforcedNoRules)

	for i := 0; i < 3; i++ {
		if err := applyPlan(plan.PlanID); err != nil {
			t.Fatalf("repeat apply #%d must keep returning the recorded correlation: %v", i+1, err)
		}
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d after repeats, want exactly 1", n)
	}
	reloaded, err := plans.Load(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Correlation.OperationID != firstOp {
		t.Fatalf("operation id changed on repeat: %q then %q", firstOp, reloaded.Correlation.OperationID)
	}
}

func TestPlanApply_ConcurrentUnderEnforcementExecutesOnce(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	writeAuthzPolicy(t, enforcedWithApproval)
	approvePlanForTest(t, plan)

	var count atomic.Int32
	prev := planExecRebuild
	planExecRebuild = func(spec *rebuildSpec, op *opRun) error {
		count.Add(1)
		op.setResult(&ops.Result{Version: 99})
		return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{App: spec.Name, Version: 99}))
	}
	t.Cleanup(func() { planExecRebuild = prev; machine.LeaveJSON() })

	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = applyPlan(plan.PlanID)
		}(i)
	}
	wg.Wait()

	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d under concurrency, want exactly 1", n)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent apply #%d failed: %v", i, err)
		}
	}
	// Exactly one authorization allow was recorded: the others saw the
	// already-applied correlation and never reached the boundary.
	decisions, _, err := authz.ListDecisions(plan.PlanID, 0)
	if err != nil {
		t.Fatal(err)
	}
	allows := 0
	for _, d := range decisions {
		if d.Decision == authz.EffectAllow {
			allows++
		}
	}
	if allows != 1 {
		t.Fatalf("recorded %d allow decisions under concurrency, want exactly 1", allows)
	}
}

// TestPlanApply_RestartAfterAuthorizationNeverDuplicates: an application
// interrupted after authorization is closed as indeterminate by the Phase 1
// ledger and never executed a second time, approval or not.
func TestPlanApply_RestartAfterAuthorizationNeverDuplicates(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	writeAuthzPolicy(t, enforcedWithApproval)
	approvePlanForTest(t, plan)

	count := withExecutionSeam(t)

	// Simulate a crash between authorization and completion: the durable
	// ledger holds an in-progress entry for the plan's key.
	if _, _, err := ops.BeginKey(planRequestKey(plan.PlanID), plan.PlanHash,
		ops.KindRebuild, plan.Action.Application); err != nil {
		t.Fatalf("BeginKey: %v", err)
	}

	if err := applyPlan(plan.PlanID); err == nil {
		t.Fatal("an interrupted application must fail closed, not execute again")
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("restart must never re-execute: seam ran %d time(s)", n)
	}
}

// --- Planless (direct) execution --------------------------------------------

// TestDirectExecution_LegacyHostIsUnchanged is the compatibility promise: a
// host that has not configured authorization behaves exactly as before.
func TestDirectExecution_LegacyHostIsUnchanged(t *testing.T) {
	planTestDataDir(t)
	for _, action := range []string{authz.ActionRebuild, authz.ActionRollback} {
		if err := authorizeDirectExecution(action, "billing", "app-1"); err != nil {
			t.Fatalf("%s on an unconfigured host must be permitted: %v", action, err)
		}
	}
}

// TestDirectExecution_DeniedUnderEnforcement: under enforced authorization,
// planless execution is refused — which is what closes the obvious bypass of
// "skip the plan, run rebuild directly". The webhook queue and the monitor
// daemon's remote rebuild both re-invoke `phelix rebuild`, so they terminate
// at this same gate.
func TestDirectExecution_DeniedUnderEnforcement(t *testing.T) {
	planTestDataDir(t)
	// Even the most permissive possible rule set does not permit a planless
	// mutation: the plan binding is not a rule, it is the boundary's shape.
	writeAuthzPolicy(t, `{"schema_version":"1","mode":"enforced","rules":[
		{"actor":{"type":"*","authenticated":false},"action":"*","target":"*","effect":"allow"}]}`)

	for _, action := range []string{authz.ActionRebuild, authz.ActionRollback} {
		err := authorizeDirectExecution(action, "billing", "app-1")
		if err == nil {
			t.Fatalf("planless %s must be denied under enforced authorization", action)
		}
		if got := phelixerr.CodeOf(err); got != phelixerr.CodeAuthzDenied {
			t.Fatalf("%s: code = %s, want AUTHZ_DENIED", action, got)
		}
		if got := ExitCodeFor(err); got != ExitPermission {
			t.Fatalf("%s: exit = %d, want %d", action, got, ExitPermission)
		}
		if !strings.Contains(err.Error(), "plan") {
			t.Fatalf("%s: the denial must tell the caller to use a plan: %q", action, err.Error())
		}
	}
}

func TestDirectExecution_FailsClosedOnBrokenPolicy(t *testing.T) {
	planTestDataDir(t)
	writeAuthzPolicy(t, `{"schema_version":"1","mode":"wide-open"}`)
	err := authorizeDirectExecution(authz.ActionRebuild, "billing", "app-1")
	if err == nil {
		t.Fatal("a broken policy must not permit execution")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeAuthzInvalid {
		t.Fatalf("code = %s, want AUTHZ_INVALID", got)
	}
}

// TestAuthzBoundary_PolicyAndApprovalAreReadPerDecision is the race-safety
// property made concrete: nothing is cached, so the state at the moment
// mutation begins is the state that decides. A cached policy or approval
// would mean a decision could be acted on after it stopped being true.
func TestAuthzBoundary_PolicyAndApprovalAreReadPerDecision(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	count := withExecutionSeam(t)

	check := func(label, wantCode string) {
		t.Helper()
		_, err := authzGate.Evaluate(contextTODO(), authz.ExecutionRequest{
			Action: plan.Action.Type,
			Target: authz.Target{App: plan.Action.Application, AppID: plan.Target.AppID},
			Plan:   &authz.PlanRef{ID: plan.PlanID, Hash: plan.PlanHash},
		})
		if err != nil {
			t.Fatalf("%s: Evaluate: %v", label, err)
		}
		out, _ := authzGate.Evaluate(contextTODO(), authz.ExecutionRequest{
			Action: plan.Action.Type,
			Target: authz.Target{App: plan.Action.Application, AppID: plan.Target.AppID},
			Plan:   &authz.PlanRef{ID: plan.PlanID, Hash: plan.PlanHash},
		})
		if out.Decision.Code != wantCode {
			t.Fatalf("%s: code = %q, want %q", label, out.Decision.Code, wantCode)
		}
	}

	// Each transition below must be observed by the very next decision.
	check("unconfigured host", "AUTHZ_ALLOWED")

	writeAuthzPolicy(t, enforcedNoRules)
	check("policy installed", "AUTHZ_DENIED")

	writeAuthzPolicy(t, enforcedWithApproval)
	check("approval now required", "APPROVAL_REQUIRED")

	approvePlanForTest(t, plan)
	check("approval granted", "AUTHZ_ALLOWED")

	if _, err := authz.RevokeApproval(plan.PlanID); err != nil {
		t.Fatal(err)
	}
	check("approval revoked", "APPROVAL_REQUIRED")

	if err := os.Remove(authz.PolicyPath()); err != nil {
		t.Fatal(err)
	}
	check("policy removed", "AUTHZ_ALLOWED")

	// None of that evaluated anything into existence.
	if n := count.Load(); n != 0 {
		t.Fatalf("evaluating the boundary executed %d time(s); it must never mutate", n)
	}
}

// TestAuthzGate_RebuildAndRollbackBothTerminateAtTheBoundary pins that the
// two direct mutation entry points share one gate. The webhook queue and the
// monitor daemon's remote rebuild re-invoke `phelix rebuild`, so covering it
// covers them; the remote rollback calls the gate in-process.
func TestAuthzGate_RebuildAndRollbackBothTerminateAtTheBoundary(t *testing.T) {
	planTestDataDir(t)
	var seen []string
	prev := authzGate
	authzGate = &authz.Gate{
		LoadPolicy: func() (*authz.Policy, error) {
			return authz.DecodePolicy([]byte(enforcedNoRules))
		},
		Record: func(req authz.Request, d authz.Decision, at int64) (string, error) {
			seen = append(seen, req.Action)
			return "azd_test", nil
		},
	}
	t.Cleanup(func() { authzGate = prev })

	for _, action := range []string{authz.ActionRebuild, authz.ActionRollback} {
		if err := authorizeDirectExecution(action, "billing", "app-1"); err == nil {
			t.Fatalf("%s must be refused", action)
		}
	}
	if len(seen) != 2 || seen[0] != authz.ActionRebuild || seen[1] != authz.ActionRollback {
		t.Fatalf("both entry points must reach the same gate, saw %v", seen)
	}
}

// contextTODO keeps the test calls readable; the authorizer ignores the
// context (it performs no I/O), and this documents that.
func contextTODO() context.Context { return context.Background() }

// --- Error contract ----------------------------------------------------------

func TestAuthzErrors_ExitCodesAndRetryability(t *testing.T) {
	cases := []struct {
		code      phelixerr.Code
		retryable bool
	}{
		{phelixerr.CodeAuthzDenied, false},
		{phelixerr.CodeAuthzInvalid, false},
		{phelixerr.CodeAuthzUnavailable, true},
		{phelixerr.CodeApprovalRequired, false},
		{phelixerr.CodeApprovalStale, false},
		{phelixerr.CodeApprovalInvalid, false},
	}
	for _, tc := range cases {
		if got := ExitCodeFor(phelixerr.New(tc.code, "x")); got != ExitPermission {
			t.Errorf("%s exit = %d, want %d", tc.code, got, ExitPermission)
		}
		if got := phelixerr.Retryable(tc.code); got != tc.retryable {
			t.Errorf("%s retryable = %t, want %t", tc.code, got, tc.retryable)
		}
	}
}

// TestAuthzFailure_MachineEnvelopeIsWellFormed pins the Phase 1 contract for
// the new failure codes: one envelope, schema 1, the real exit code, explicit
// retryability.
func TestAuthzFailure_MachineEnvelopeIsWellFormed(t *testing.T) {
	for _, code := range []phelixerr.Code{
		phelixerr.CodeAuthzDenied, phelixerr.CodeAuthzUnavailable, phelixerr.CodeAuthzInvalid,
		phelixerr.CodeApprovalRequired, phelixerr.CodeApprovalStale, phelixerr.CodeApprovalInvalid,
	} {
		err := phelixerr.New(code, "refused")
		env := machine.Failure(err, ExitCodeFor(err), "")
		if env.SchemaVersion != machine.SchemaVersion {
			t.Fatalf("%s: schema_version = %q, want %q", code, env.SchemaVersion, machine.SchemaVersion)
		}
		if env.Status != machine.StatusFailed || env.Error == nil {
			t.Fatalf("%s: envelope is not a failure: %+v", code, env)
		}
		if env.Error.Code != code.String() {
			t.Fatalf("%s: error code = %q", code, env.Error.Code)
		}
		if env.Error.ExitCode != ExitPermission {
			t.Fatalf("%s: exit_code = %d, want %d", code, env.Error.ExitCode, ExitPermission)
		}
		if env.Error.Retryable == nil {
			t.Fatalf("%s: retryable must be explicit for a classified authorization failure", code)
		}
		if *env.Error.Retryable != phelixerr.Retryable(code) {
			t.Fatalf("%s: retryable = %t, disagrees with the shared classification", code, *env.Error.Retryable)
		}
	}
}

// TestAuthzFailure_NoSecretsInResponsesOrRecords: the authorization path must
// not become a leak surface. The reasons are authored constants, but a
// credential-shaped string pushed through the boundary must still come out
// redacted everywhere it lands.
func TestAuthzFailure_NoSecretsInResponsesOrRecords(t *testing.T) {
	planTestDataDir(t)
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

	// Machine error envelope.
	env := machine.Failure(phelixerr.Newf(phelixerr.CodeAuthzDenied,
		"denied for token=%s", secret), ExitPermission, "")
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatalf("the machine envelope leaked a credential: %s", blob)
	}

	// Audit record.
	if _, err := authz.RecordDecision(
		authz.Request{
			Actor:  authz.Actor{Type: authz.CallerCLI},
			Action: authz.ActionRebuild,
			Target: authz.Target{App: "billing"},
			Plan:   &authz.PlanRef{ID: "pln_000000000000000a", Hash: "sha256:aaaa"},
		},
		authz.Decision{Effect: authz.EffectDeny, Code: phelixerr.CodeAuthzDenied.String(),
			Reason: "denied for token=" + secret, Mode: authz.ModeEnforced, RuleIndex: -1},
		1); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	raw, err := os.ReadFile(authz.DecisionsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the decision log leaked a credential: %s", raw)
	}

	// Approval artifact: there is nowhere in the schema to put a secret, and
	// the one free-text field (provenance) is redacted.
	approver, err := authz.LocalCLIAuthenticator{}.Authenticate()
	if err != nil {
		t.Fatal(err)
	}
	a, err := authz.NewApproval(approver,
		authz.PlanRef{ID: "pln_000000000000000a", Hash: "sha256:aaaa"},
		authz.ActionRebuild, "billing", "operator token="+secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := authz.SaveApproval(a); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(authz.ApprovalsDir(), "pln_000000000000000a.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the approval artifact leaked a credential: %s", raw)
	}
}

// TestAuthzGate_NoCallerSuppliedActorFlag is Invariant 10 at the CLI
// boundary: Phelix accepts no flag through which a caller could name itself
// an authorized actor, on any command that can mutate or approve.
func TestAuthzGate_NoCallerSuppliedActorFlag(t *testing.T) {
	forbidden := map[string]bool{
		"actor": true, "actor-id": true, "as": true,
		"principal": true, "authenticated": true, "role": true, "user": true,
	}
	var check func(c *cobra.Command)
	check = func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if forbidden[f.Name] {
				t.Fatalf("%q exposes a caller-supplied identity flag --%s; identity must come "+
					"from an authenticator, never from the command line", c.CommandPath(), f.Name)
			}
		})
		for _, sub := range c.Commands() {
			check(sub)
		}
	}
	for _, cmd := range []*cobra.Command{RebuildCmd, RollbackCmd, PlanCmd, AuthzCmd} {
		check(cmd)
	}

	// And the only actor the CLI can produce is the unauthenticated local one.
	actor, err := authz.LocalCLIAuthenticator{}.Authenticate()
	if err != nil {
		t.Fatal(err)
	}
	if actor.Authenticated || actor.ID != "" {
		t.Fatalf("the CLI authenticator produced a privileged actor: %+v", actor)
	}
}
