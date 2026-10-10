package cmd

// session_apply_test.go — in-process acceptance of the correlation/recovery
// story: a plan applied through the REAL pipeline (execution seam) is linked to
// a session, and `session show` resolves the plan and the resulting operation
// to their real current state, proving the session correlates existing work
// without copying or owning its authoritative state.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/abdorrahmani/phelix/internal/machine"
	phelixmcp "github.com/abdorrahmani/phelix/internal/mcp"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/plans"
)

type resolvedRefView struct {
	ID      string `json:"id"`
	Present bool   `json:"present"`
	State   string `json:"state"`
	Status  string `json:"status"`
}

type resolvedView struct {
	Plans       []resolvedRefView `json:"plans"`
	Operations  []resolvedRefView `json:"operations"`
	Deployments []resolvedRefView `json:"deployments"`
}

func decodeResolved(t *testing.T, env *machine.Envelope) resolvedView {
	t.Helper()
	raw, err := json.Marshal(env.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var v struct {
		Resolved resolvedView `json:"resolved"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode resolved: %v", err)
	}
	return v.Resolved
}

func TestSessionCorrelatesAppliedPlanAndOperation(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	count := withExecutionSeam(t)
	withMCPAuthenticator(t)
	writeEnforcedPolicy(t, info.Name, false) // allow mcp, no approval required

	ctx := context.Background()
	var svc cmdServices

	// Apply through the real pipeline; capture the operation id it correlates.
	applied := checkEnv(t)(svc.PlanApply(ctx, phelixmcp.PlanApplyInput{PlanID: plan.PlanID}))
	if applied.Status != machine.StatusSucceeded {
		t.Fatalf("apply = %+v, want succeeded", applied)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1", n)
	}
	opID := applied.OperationID
	if opID == "" {
		t.Fatal("applied-plan envelope carries no operation_id to correlate")
	}

	// Track it: a session links both the plan and the resulting operation.
	sv := decodeSessionResult(t, checkEnv(t)(svc.SessionCreate(ctx, phelixmcp.SessionCreateInput{App: info.Name})))
	if env := checkEnv(t)(svc.SessionCheckpoint(ctx, phelixmcp.SessionCheckpointInput{
		SessionID: sv.SessionID, Plans: []string{plan.PlanID}, Operations: []string{opID}, Note: "applied",
	})); env.Status != machine.StatusSucceeded {
		t.Fatalf("linking applied work to the session = %+v", env)
	}

	// The authoritative state lives in the plan/op stores, not the session.
	if rec, err := ops.Load(opID); err != nil || rec.Status != machine.StatusSucceeded {
		t.Fatalf("operation %s not independently succeeded: rec=%+v err=%v", opID, rec, err)
	}
	if p, err := plans.Load(plan.PlanID); err != nil || p.Status != plans.StatusApplied {
		t.Fatalf("plan %s not independently applied: status=%v err=%v", plan.PlanID, p.Status, err)
	}

	// `session show` resolves both references to their current state honestly.
	resolved := decodeResolved(t, checkEnv(t)(svc.SessionShow(ctx, phelixmcp.SessionShowInput{SessionID: sv.SessionID})))
	if len(resolved.Plans) != 1 || resolved.Plans[0].State != "present" || resolved.Plans[0].Status != plans.StatusApplied {
		t.Fatalf("resolved plan = %+v, want present/applied", resolved.Plans)
	}
	if len(resolved.Operations) != 1 || resolved.Operations[0].State != "present" || resolved.Operations[0].Status != machine.StatusSucceeded {
		t.Fatalf("resolved operation = %+v, want present/succeeded", resolved.Operations)
	}
}
