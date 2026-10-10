package cmd

// mcp_test.go — behavior and security tests for the MCP adapter's Services
// implementation (cmdServices). These exercise the REAL reuse path: machine
// capture around the actual command logic, the Phase 4 authorization boundary
// reached as the MCP caller, approvals, idempotency and redaction. They reuse
// the Phase 3 plan fixtures (planTestDataDir / withAppStore / manualRebuildPlan
// / withExecutionSeam) from plans_test.go.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/authz"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	phelixmcp "github.com/abdorrahmani/phelix/internal/mcp"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/plans"
	"github.com/abdorrahmani/phelix/internal/session"
)

// withMCPAuthenticator installs the MCP authenticator on the process-wide gate
// (what `phelix mcp serve` does) and restores the previous one. Every tool call
// in the serve process authorizes as the local, unauthenticated MCP caller.
func withMCPAuthenticator(t *testing.T) {
	t.Helper()
	prev := authzGate.Authenticator
	authzGate.Authenticator = authz.LocalMCPAuthenticator{}
	t.Cleanup(func() { authzGate.Authenticator = prev })
}

// writeEnforcedPolicy writes an enforced host policy allowing the mcp caller to
// act on appName, optionally requiring a plan-bound approval.
func writeEnforcedPolicy(t *testing.T, appName string, requireApproval bool) {
	t.Helper()
	authFalse := false
	rule := authz.Rule{
		Actor:           authz.ActorMatch{Type: authz.CallerMCP, Authenticated: &authFalse},
		Action:          "*",
		Target:          appName,
		Effect:          "allow",
		RequireApproval: requireApproval,
	}
	body := map[string]any{
		"schema_version": authz.SchemaVersion,
		"mode":           authz.ModeEnforced,
		"rules":          []authz.Rule{rule},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if err := os.MkdirAll(authz.Dir(), 0o700); err != nil {
		t.Fatalf("mkdir authz: %v", err)
	}
	if err := os.WriteFile(authz.PolicyPath(), data, 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
}

// approvePlan creates and stores a valid plan-bound approval, exactly as
// `phelix authz approve` would.
func approvePlan(t *testing.T, p *plans.Plan) {
	t.Helper()
	ap, err := authz.NewApproval(
		authz.Actor{Type: authz.CallerCLI},
		authz.PlanRef{ID: p.PlanID, Hash: p.PlanHash},
		p.Action.Type, p.Action.Application, "test",
	)
	if err != nil {
		t.Fatalf("NewApproval: %v", err)
	}
	if err := authz.SaveApproval(ap); err != nil {
		t.Fatalf("SaveApproval: %v", err)
	}
}

// checkEnv returns an asserting wrapper: checkEnv(t)(svc.Call(...)) fails the
// test on a transport error and returns the schema-checked envelope. The
// curried form lets a two-value Services call be passed as one argument group.
func checkEnv(t *testing.T) func(*machine.Envelope, error) *machine.Envelope {
	return func(env *machine.Envelope, err error) *machine.Envelope {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		if env == nil {
			t.Fatal("nil envelope")
		}
		if env.SchemaVersion != machine.SchemaVersion {
			t.Fatalf("schema_version = %q, want %q", env.SchemaVersion, machine.SchemaVersion)
		}
		return env
	}
}

func TestMCPServices_ReadToolsReuseEnvelopes(t *testing.T) {
	planTestDataDir(t)
	ctx := context.Background()
	var svc cmdServices

	// A read with no app: runtime inspection always succeeds.
	env := checkEnv(t)(svc.Inspect(ctx, phelixmcp.InspectInput{Resource: phelixmcp.ResourceRuntime}))
	if env.Status != machine.StatusSucceeded {
		t.Fatalf("inspect runtime status = %q, want succeeded (err=%+v)", env.Status, env.Error)
	}

	// A well-formed but absent plan id fails closed as NOT_FOUND — a failure
	// envelope, never a transport error and never a fake success.
	env = checkEnv(t)(svc.PlanShow(ctx, phelixmcp.PlanShowInput{PlanID: "pln_0000000000000000"}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeNotFound.String() {
		t.Fatalf("plan show (absent) = %+v, want failed/%s", env, phelixerr.CodeNotFound)
	}

	// A malformed id is an invalid argument, surfaced as a failure envelope.
	env = checkEnv(t)(svc.PlanShow(ctx, phelixmcp.PlanShowInput{PlanID: "not-a-plan-id"}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeInvalidArgument.String() {
		t.Fatalf("plan show (malformed) = %+v, want failed/%s", env, phelixerr.CodeInvalidArgument)
	}

	// A bounded, empty plan list succeeds.
	env = checkEnv(t)(svc.PlanList(ctx, phelixmcp.PlanListInput{}))
	if env.Status != machine.StatusSucceeded {
		t.Fatalf("plan list status = %q, want succeeded", env.Status)
	}

	// An unrecognized operation id is an invalid argument.
	env = checkEnv(t)(svc.OperationStatus(ctx, phelixmcp.OperationStatusInput{OperationID: "bogus"}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeInvalidArgument.String() {
		t.Fatalf("operation status (bogus) = %+v, want failed/%s", env, phelixerr.CodeInvalidArgument)
	}
}

func TestMCPServices_OverCapLimitIsRejected(t *testing.T) {
	planTestDataDir(t)
	env := checkEnv(t)(cmdServices{}.PlanList(context.Background(), phelixmcp.PlanListInput{Limit: phelixmcp.MaxPlanList + 1}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeInvalidArgument.String() {
		t.Fatalf("over-cap plan list = %+v, want failed/%s", env, phelixerr.CodeInvalidArgument)
	}
}

// TestMCPServices_NoStdoutContamination proves a captured command never writes
// to the process stdout (which, in the server, carries the MCP protocol):
// progress is detoured to stderr and the envelope is returned, not printed.
func TestMCPServices_NoStdoutContamination(t *testing.T) {
	planTestDataDir(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	_, svcErr := cmdServices{}.Context(context.Background(), phelixmcp.ContextInput{})
	os.Stdout = orig
	_ = w.Close()
	out, _ := drainPipe(r)
	if svcErr != nil {
		t.Fatalf("Context: %v", svcErr)
	}
	if len(out) != 0 {
		t.Fatalf("stdout was contaminated with %d bytes: %q", len(out), string(out))
	}
}

func TestMCPServices_PlanCreateReusesPipeline(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	env := checkEnv(t)(cmdServices{}.PlanCreate(context.Background(), phelixmcp.PlanCreateInput{
		Action: phelixmcp.ActionRebuild,
		App:    info.Name,
	}))
	if env.Status != machine.StatusSucceeded {
		t.Fatalf("plan create status = %q, want succeeded (err=%+v)", env.Status, env.Error)
	}
	// Creation persists an immutable plan but never executes it.
	list, _, err := plans.List(info.Name, 0)
	if err != nil {
		t.Fatalf("plans.List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("plan count = %d, want 1 (create must persist exactly one plan)", len(list))
	}
	if list[0].Status != plans.StatusCreated {
		t.Fatalf("created plan status = %q, want created (must not auto-apply)", list[0].Status)
	}
}

// drainPipe drains r fully (helper to avoid pulling in io in the test's main body).
func drainPipe(r *os.File) ([]byte, error) {
	defer r.Close()
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// TestMCPServices_PlanApplyDeniedOnLegacyHost proves the MCP caller does NOT
// inherit the legacy-local trust the CLI has: on a host with no policy, an
// MCP-originated protected execution is denied, and nothing mutates.
func TestMCPServices_PlanApplyDeniedOnLegacyHost(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	count := withExecutionSeam(t)
	withMCPAuthenticator(t)

	env := checkEnv(t)(cmdServices{}.PlanApply(context.Background(), phelixmcp.PlanApplyInput{PlanID: plan.PlanID}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeAuthzDenied.String() {
		t.Fatalf("legacy-host MCP apply = %+v, want failed/%s", env, phelixerr.CodeAuthzDenied)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: execution seam ran %d time(s) on a denied apply", n)
	}
	if reloaded, err := plans.Load(plan.PlanID); err == nil && reloaded.Status == plans.StatusApplied {
		t.Fatal("denied plan must never be marked applied")
	}
}

// TestMCPServices_PlanApplyApprovalGate proves the full plan-bound approval
// gate over the MCP path: refused without an approval, executed once a valid
// approval exists — and nothing mutates while it is refused.
func TestMCPServices_PlanApplyApprovalGate(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	count := withExecutionSeam(t)
	withMCPAuthenticator(t)
	writeEnforcedPolicy(t, info.Name, true)

	env := checkEnv(t)(cmdServices{}.PlanApply(context.Background(), phelixmcp.PlanApplyInput{PlanID: plan.PlanID}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeApprovalRequired.String() {
		t.Fatalf("apply without approval = %+v, want failed/%s", env, phelixerr.CodeApprovalRequired)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: seam ran %d time(s) before approval", n)
	}

	approvePlan(t, plan)
	env = checkEnv(t)(cmdServices{}.PlanApply(context.Background(), phelixmcp.PlanApplyInput{PlanID: plan.PlanID}))
	if env.Status != machine.StatusSucceeded {
		t.Fatalf("apply with valid approval = %+v, want succeeded", env)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("approved apply execution count = %d, want exactly 1", n)
	}
}

// TestMCPServices_ApprovalDoesNotTransfer proves an approval for one plan can
// never authorize another, even for the same app and action.
func TestMCPServices_ApprovalDoesNotTransfer(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	planA := manualRebuildPlan(t, info.Name, info.ID, nil)
	planB := manualRebuildPlan(t, info.Name, info.ID, []plans.Precondition{
		{Type: plans.PreconditionAppExists, Expected: info.ID, Source: "apps.json"},
	})
	count := withExecutionSeam(t)
	withMCPAuthenticator(t)
	writeEnforcedPolicy(t, info.Name, true)

	approvePlan(t, planA) // only plan A is approved

	env := checkEnv(t)(cmdServices{}.PlanApply(context.Background(), phelixmcp.PlanApplyInput{PlanID: planB.PlanID}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodeApprovalRequired.String() {
		t.Fatalf("apply B with A's approval = %+v, want failed/%s", env, phelixerr.CodeApprovalRequired)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: seam ran %d time(s) applying an unapproved plan", n)
	}
}

// TestMCPServices_PlanApplyIdempotent proves request-key idempotency is
// preserved over the MCP path: repeated applies never re-execute.
func TestMCPServices_PlanApplyIdempotent(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	count := withExecutionSeam(t)
	withMCPAuthenticator(t)
	writeEnforcedPolicy(t, info.Name, false) // allow, no approval required

	for i := 0; i < 3; i++ {
		env := checkEnv(t)(cmdServices{}.PlanApply(context.Background(), phelixmcp.PlanApplyInput{PlanID: plan.PlanID}))
		if env.Status != machine.StatusSucceeded {
			t.Fatalf("apply #%d = %+v, want succeeded", i+1, env)
		}
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d after three MCP applies, want exactly 1", n)
	}
}

// TestMCPServices_RejectsTamperedPlan proves a hash-mismatched plan fails
// closed over the MCP path with zero mutation.
func TestMCPServices_RejectsTamperedPlan(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)

	tampered := *plan
	tampered.Inputs.Port = 9999
	data, _ := json.Marshal(&tampered)
	if err := os.WriteFile(filepath.Join(plans.Dir(), plan.PlanID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	count := withExecutionSeam(t)
	withMCPAuthenticator(t)
	writeEnforcedPolicy(t, info.Name, false)

	env := checkEnv(t)(cmdServices{}.PlanApply(context.Background(), phelixmcp.PlanApplyInput{PlanID: plan.PlanID}))
	if env.Status != machine.StatusFailed || env.Error == nil || env.Error.Code != phelixerr.CodePlanHashMismatch.String() {
		t.Fatalf("tampered plan apply = %+v, want failed/%s", env, phelixerr.CodePlanHashMismatch)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("MUTATION HAPPENED: seam ran %d time(s) on a tampered plan", n)
	}
}

// TestMCPServices_ReadRunsDuringLongApply proves Task 3: a read-only session
// tool returns while a long mutating plan_apply is still in flight on the same
// process, instead of blocking behind it on the capture lock.
func TestMCPServices_ReadRunsDuringLongApply(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)
	withMCPAuthenticator(t)
	writeEnforcedPolicy(t, info.Name, false) // allow, no approval required

	// A session to read, created before the apply starts.
	s, err := session.Create(session.CreateOpts{Title: "track", Actor: machine.CLIActor()})
	if err != nil {
		t.Fatalf("session.Create: %v", err)
	}

	// A blocking execution seam: the apply parks inside it, holding the capture
	// lock, until the test releases it.
	started := make(chan struct{})
	release := make(chan struct{})
	prev := planExecRebuild
	planExecRebuild = func(spec *rebuildSpec, op *opRun) error {
		close(started)
		<-release
		op.setResult(&ops.Result{Version: 1})
		return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{App: spec.Name, Version: 1}))
	}
	t.Cleanup(func() { planExecRebuild = prev; machine.LeaveJSON() })

	ctx := context.Background()
	applyDone := make(chan *machine.Envelope, 1)
	go func() {
		env, _ := cmdServices{}.PlanApply(ctx, phelixmcp.PlanApplyInput{PlanID: plan.PlanID})
		applyDone <- env
	}()
	<-started // the apply is now parked inside the seam, holding mcpCaptureMu

	readDone := make(chan *machine.Envelope, 1)
	go func() {
		env, _ := cmdServices{}.SessionShow(ctx, phelixmcp.SessionShowInput{SessionID: s.SessionID})
		readDone <- env
	}()
	select {
	case env := <-readDone:
		if env == nil || env.Status != machine.StatusSucceeded {
			t.Fatalf("session_show during apply = %+v, want succeeded", env)
		}
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("session_show blocked behind the in-flight apply (Task 3 regression)")
	}

	close(release)
	applyEnv := <-applyDone
	if applyEnv == nil || applyEnv.Status != machine.StatusSucceeded {
		t.Fatalf("apply = %+v, want succeeded", applyEnv)
	}
}

// TestMCPInputsCarryNoCallerIdentity proves the tool input schemas give a
// client no way to assert an actor, role, authentication state, or
// authorization/approval decision — identity is produced only by the
// authenticator, never from the wire.
func TestMCPInputsCarryNoCallerIdentity(t *testing.T) {
	forbidden := []string{"actor", "role", "authenticated", "approval", "approved", "authoriz", "identity", "principal", "token", "credential"}
	types := []reflect.Type{
		reflect.TypeOf(phelixmcp.ContextInput{}),
		reflect.TypeOf(phelixmcp.InspectInput{}),
		reflect.TypeOf(phelixmcp.PlanShowInput{}),
		reflect.TypeOf(phelixmcp.PlanListInput{}),
		reflect.TypeOf(phelixmcp.OperationStatusInput{}),
		reflect.TypeOf(phelixmcp.PlanCreateInput{}),
		reflect.TypeOf(phelixmcp.PlanApplyInput{}),
	}
	for _, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			for _, bad := range forbidden {
				if strings.Contains(name, bad) {
					t.Fatalf("%s.%s looks like a caller-supplied identity/authorization field (contains %q); MCP clients must never supply those", typ.Name(), typ.Field(i).Name, bad)
				}
			}
		}
	}
}
