package cmd

// session_test.go — tests for the session command group via the MCP Services
// reuse path (cmdServices), proving CLI and MCP share one implementation, that
// provenance is server-derived (cli vs mcp, never from the wire), and that a
// session reference never bypasses the Phase 4 authorization/approval gate.
// Reuses the plan fixtures (planTestDataDir / withAppStore / manualRebuildPlan
// / withExecutionSeam) and MCP helpers (withMCPAuthenticator / writeEnforcedPolicy)
// from plans_test.go and mcp_test.go.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	phelixmcp "github.com/abdorrahmani/phelix/internal/mcp"
)

type sessionResultView struct {
	SessionID string         `json:"session_id"`
	Status    string         `json:"status"`
	Rev       int            `json:"rev"`
	Actor     *machine.Actor `json:"actor"`
}

func decodeSessionResult(t *testing.T, env *machine.Envelope) sessionResultView {
	t.Helper()
	raw, err := json.Marshal(env.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var v sessionResultView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode session result: %v", err)
	}
	return v
}

func TestMCPServices_SessionReuse(t *testing.T) {
	planTestDataDir(t)
	withMCPAuthenticator(t)
	ctx := context.Background()
	var svc cmdServices

	env := checkEnv(t)(svc.SessionCreate(ctx, phelixmcp.SessionCreateInput{Title: "work", App: "billing"}))
	if env.Status != machine.StatusSucceeded {
		t.Fatalf("session create = %+v, want succeeded", env)
	}
	if env.OperationID != "" {
		t.Fatalf("a session is not an operation; envelope must carry no operation_id, got %q", env.OperationID)
	}
	created := decodeSessionResult(t, env)
	if created.Status != "active" || created.Rev != 1 {
		t.Fatalf("created session status=%q rev=%d, want active/1", created.Status, created.Rev)
	}
	if created.Actor == nil || created.Actor.Type != "mcp" {
		t.Fatalf("mcp-created session actor = %+v, want type mcp", created.Actor)
	}
	id := created.SessionID

	if env := checkEnv(t)(svc.SessionShow(ctx, phelixmcp.SessionShowInput{SessionID: id})); env.Status != machine.StatusSucceeded {
		t.Fatalf("session show = %+v", env)
	}
	if env := checkEnv(t)(svc.SessionList(ctx, phelixmcp.SessionListInput{})); env.Status != machine.StatusSucceeded {
		t.Fatalf("session list = %+v", env)
	}
	if env := checkEnv(t)(svc.SessionCheckpoint(ctx, phelixmcp.SessionCheckpointInput{SessionID: id, Note: "progress"})); env.Status != machine.StatusSucceeded {
		t.Fatalf("session checkpoint = %+v", env)
	}
	done := checkEnv(t)(svc.SessionComplete(ctx, phelixmcp.SessionCompleteInput{SessionID: id, Result: "shipped"}))
	if v := decodeSessionResult(t, done); v.Status != "completed" {
		t.Fatalf("completed session status = %q, want completed", v.Status)
	}
}

func TestSessionCLIActorProvenance(t *testing.T) {
	planTestDataDir(t)
	env := checkEnv(t)(cmdServices{}.SessionCreate(context.Background(), phelixmcp.SessionCreateInput{Title: "x"}))
	v := decodeSessionResult(t, env)
	if v.Actor == nil || v.Actor.Type != "cli" {
		t.Fatalf("default session actor = %+v, want type cli", v.Actor)
	}
}

// TestSessionLinkDoesNotAuthorize is the central security property: linking a
// plan to a session confers NO execution right. An approval-required plan still
// refuses to apply after being referenced by a session, and nothing executes.
func TestSessionLinkDoesNotAuthorize(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	count := withExecutionSeam(t)
	withMCPAuthenticator(t)
	writeEnforcedPolicy(t, info.Name, true) // allow mcp, require approval

	ctx := context.Background()
	var svc cmdServices
	sv := decodeSessionResult(t, checkEnv(t)(svc.SessionCreate(ctx, phelixmcp.SessionCreateInput{App: info.Name})))
	if env := checkEnv(t)(svc.SessionCheckpoint(ctx, phelixmcp.SessionCheckpointInput{SessionID: sv.SessionID, Plans: []string{plan.PlanID}, Note: "planned"})); env.Status != machine.StatusSucceeded {
		t.Fatalf("linking the plan to the session = %+v, want succeeded", env)
	}
	env := checkEnv(t)(svc.PlanApply(ctx, phelixmcp.PlanApplyInput{PlanID: plan.PlanID}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeApprovalRequired.String() {
		t.Fatalf("apply after session link = %+v, want failed/%s (a reference is not an approval)", env, phelixerr.CodeApprovalRequired)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: execution seam ran %d time(s) — a session reference must never authorize", n)
	}
}

func TestSessionMCPInputsCarryNoCallerIdentity(t *testing.T) {
	forbidden := []string{"actor", "role", "authenticated", "approval", "approved", "authoriz", "identity", "principal", "token", "credential"}
	types := []reflect.Type{
		reflect.TypeOf(phelixmcp.SessionCreateInput{}),
		reflect.TypeOf(phelixmcp.SessionShowInput{}),
		reflect.TypeOf(phelixmcp.SessionListInput{}),
		reflect.TypeOf(phelixmcp.SessionCheckpointInput{}),
		reflect.TypeOf(phelixmcp.SessionCompleteInput{}),
		reflect.TypeOf(phelixmcp.SessionFailInput{}),
	}
	for _, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			for _, bad := range forbidden {
				if strings.Contains(name, bad) {
					t.Fatalf("%s.%s looks like a caller-supplied identity/authorization field (contains %q)", typ.Name(), typ.Field(i).Name, bad)
				}
			}
		}
	}
}
