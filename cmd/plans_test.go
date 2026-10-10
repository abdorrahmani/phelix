package cmd

// plans_test.go — Phase 3 command-level contract tests.
//
// The critical suite is TestPlanApply_MutationSafety: for every fail-closed
// path, the execution seam must be invoked ZERO times — the test proves no
// mutation happens, not merely that an error was returned (spec §37).
//
// Isolation notes: internal/app resolves its state file eagerly at package
// init, so in-process tests cannot redirect it with t.Setenv. The app-store
// tests therefore seed a uniquely named app and restore the original
// apps.json bytes afterwards (withAppStore); everything else (plan store,
// ops ledger, versions) resolves the data dir per call and honors
// PHELIX_DATA_DIR. The full end-to-end contract runs as a subprocess test
// (TestPlanCLI_EndToEnd) with an isolated HOME, mirroring the repo's
// cli_integration_test.go pattern.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/plans"
)

func planTestDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	return dir
}

// withAppStore seeds a uniquely named app and restores the original
// apps.json bytes when the test finishes — byte-for-byte, including absence.
func withAppStore(t *testing.T) *app.AppInfo {
	t.Helper()
	stateFile := filepath.Join(appDataDirForTest(), "apps.json")
	original, readErr := os.ReadFile(stateFile)

	if err := app.Manager.LoadState(); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	name := fmt.Sprintf("pln-test-%d", time.Now().UnixNano()%1e9)
	id := app.Manager.GenerateAppID()
	info := &app.AppInfo{
		ID:        id,
		Name:      name,
		Status:    "stopped",
		Directory: t.TempDir(),
		Language:  string(builder.Go),
		Port:      8080,
	}
	app.Manager.(*app.AppManager).Apps[id] = info
	if err := app.Manager.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	t.Cleanup(func() {
		if readErr != nil && os.IsNotExist(readErr) {
			_ = os.Remove(stateFile)
			return
		}
		_ = os.WriteFile(stateFile, original, 0o644)
	})
	return info
}

// appDataDirForTest mirrors internal/app's eager resolution so the cleanup
// restores the same file the manager reads.
func appDataDirForTest() string {
	if v := os.Getenv("PHELIX_DATA_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".phelix")
}

// manualRebuildPlan assembles and persists a rebuild plan for the given app
// without going through the app-store-dependent resolver. Preconditions are
// chosen so the plan is applicable in a fresh data dir.
func manualRebuildPlan(t *testing.T, appName, appID string, preconditions []plans.Precondition) *plans.Plan {
	t.Helper()
	p := &plans.Plan{
		Status: plans.StatusCreated,
		Action: plans.Action{Type: plans.ActionRebuild, Application: appName},
		Target: plans.Target{AppID: appID, AppName: appName, Language: "go"},
		Inputs: plans.Inputs{
			SourceDir:         t.TempDir(),
			Port:              8080,
			ConfigFingerprint: plansFingerprintForTest(t, appName),
		},
		Execution:     plans.Execution{Strategy: "classic", PublicPort: 8080},
		Preconditions: preconditions,
		Capabilities:  []string{"deployment"},
	}
	if err := p.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if err := plans.Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return p
}

func plansFingerprintForTest(t *testing.T, appName string) string {
	t.Helper()
	fp, err := plans.RebuildConfigFingerprint(appName, t.TempDir())
	if err != nil {
		t.Fatalf("RebuildConfigFingerprint: %v", err)
	}
	return fp
}

// withExecutionSeam swaps the rebuild execution seam for a counting stub and
// restores it afterwards. The stub behaves like a successful deploy.
func withExecutionSeam(t *testing.T) *atomic.Int32 {
	t.Helper()
	var count atomic.Int32
	prev := planExecRebuild
	planExecRebuild = func(spec *rebuildSpec, op *opRun) error {
		count.Add(1)
		op.setResult(&ops.Result{Version: 99, Port: spec.Port, Strategy: spec.Strategy})
		return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{
			App: spec.Name, Version: 99, Port: spec.Port, Strategy: spec.Strategy,
		}))
	}
	t.Cleanup(func() {
		planExecRebuild = prev
		machine.LeaveJSON()
	})
	return &count
}

func TestPlanApply_MutationSafety(t *testing.T) {
	cases := []struct {
		name     string
		preconds func(t *testing.T, appName, appID string) []plans.Precondition
		sabotage func(t *testing.T, plan *plans.Plan)
		wantCode string
	}{
		{
			name: "stale plan (planned version never deployed)",
			preconds: func(t *testing.T, appName, appID string) []plans.Precondition {
				return []plans.Precondition{
					{Type: plans.PreconditionCurrentVersion, Expected: "v5", Source: "versions.json"},
				}
			},
			sabotage: func(t *testing.T, plan *plans.Plan) {},
			wantCode: "PLAN_STALE",
		},
		{
			name: "stale plan after version drift",
			preconds: func(t *testing.T, appName, appID string) []plans.Precondition {
				return []plans.Precondition{
					{Type: plans.PreconditionCurrentVersion, Expected: "none", Source: "versions.json"},
				}
			},
			sabotage: func(t *testing.T, plan *plans.Plan) {
				// Deploy v1 behind the plan's back (real recording path).
				bin := filepath.Join(t.TempDir(), "binary")
				if err := os.WriteFile(bin, []byte("bin"), 0o755); err != nil {
					t.Fatal(err)
				}
				rec, err := deploy.RecordFreshBuild(plan.Action.Application, "seed", bin, "", "", nil,
					deploy.DefaultRetention{Max: 5}, &colorLogger{})
				if err != nil {
					t.Fatal(err)
				}
				if err := deploy.PromoteVersion(plan.Action.Application, rec.Version, "classic"); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "PLAN_STALE",
		},
		{
			name: "tampered plan (hash mismatch)",
			preconds: func(t *testing.T, appName, appID string) []plans.Precondition {
				return nil
			},
			sabotage: func(t *testing.T, plan *plans.Plan) {
				tampered := *plan
				tampered.Inputs.Port = 9999
				data, _ := json.Marshal(&tampered)
				if err := os.WriteFile(filepath.Join(plans.Dir(), plan.PlanID+".json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "PLAN_HASH_MISMATCH",
		},
		{
			name: "corrupt plan",
			preconds: func(t *testing.T, appName, appID string) []plans.Precondition {
				return nil
			},
			sabotage: func(t *testing.T, plan *plans.Plan) {
				if err := os.WriteFile(filepath.Join(plans.Dir(), plan.PlanID+".json"), []byte("{junk"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "PLAN_CORRUPT",
		},
		{
			name: "missing capability",
			preconds: func(t *testing.T, appName, appID string) []plans.Precondition {
				return nil
			},
			sabotage: func(t *testing.T, plan *plans.Plan) {
				// Re-persist an otherwise identical plan requiring a
				// capability this build does not have.
				p := *plan
				p.Capabilities = []string{"time_travel"}
				if err := p.Finalize(); err != nil {
					t.Fatal(err)
				}
				_ = os.Remove(filepath.Join(plans.Dir(), plan.PlanID+".json"))
				if err := plans.Save(&p); err != nil {
					t.Fatal(err)
				}
				*plan = p
			},
			wantCode: "PLAN_CAPABILITY_MISSING",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planTestDataDir(t)
			info := withAppStore(t)
			plan := manualRebuildPlan(t, info.Name, info.ID, tc.preconds(t, info.Name, info.ID))
			tc.sabotage(t, plan)

			count := withExecutionSeam(t)
			err := applyPlan(plan.PlanID)
			if err == nil {
				t.Fatal("sabotaged plan must not apply")
			}
			if got := phelixerr.CodeOf(err).String(); got != tc.wantCode {
				t.Fatalf("code = %q, want %q (err: %v)", got, tc.wantCode, err)
			}
			if n := count.Load(); n != 0 {
				t.Fatalf("MUTATION HAPPENED: execution seam invoked %d time(s) on a rejected plan", n)
			}
			if reloaded, loadErr := plans.Load(plan.PlanID); loadErr == nil && reloaded.Status == plans.StatusApplied {
				t.Fatal("rejected plan must never be marked applied")
			}
			if got := ExitCodeFor(err); got != ExitValidation {
				t.Fatalf("plan validation failure exit = %d, want %d", got, ExitValidation)
			}
		})
	}
}

func TestPlanApply_SuccessExecutesAndCorrelates(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)

	count := withExecutionSeam(t)
	if err := applyPlan(plan.PlanID); err != nil {
		t.Fatalf("applyPlan: %v", err)
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d, want exactly 1", n)
	}

	loaded, err := plans.Load(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != plans.StatusApplied {
		t.Fatalf("status = %q, want applied", loaded.Status)
	}
	if loaded.Correlation == nil || loaded.Correlation.OperationID == "" {
		t.Fatal("successful application must record operation correlation")
	}

	// The operation record answers "which plan produced this operation?".
	recs, _, err := ops.List("", 0)
	if err != nil {
		t.Fatalf("ops.List: %v", err)
	}
	var found *ops.Record
	for _, r := range recs {
		if r.ID == loaded.Correlation.OperationID {
			found = r
		}
	}
	if found == nil {
		t.Fatal("operation record not found for the correlated operation id")
	}
	if found.PlanID != plan.PlanID || found.PlanHash != plan.PlanHash {
		t.Fatalf("operation record lost plan correlation: plan=%q hash=%q", found.PlanID, found.PlanHash)
	}
}

func TestPlanApply_RepeatedAfterSuccessNeverDuplicates(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)

	count := withExecutionSeam(t)
	for i := 0; i < 3; i++ {
		if err := applyPlan(plan.PlanID); err != nil {
			t.Fatalf("apply #%d: %v", i+1, err)
		}
	}
	if n := count.Load(); n != 1 {
		t.Fatalf("execution count = %d after three applies, want exactly 1", n)
	}
}

func TestPlanApply_ConcurrentIsSafe(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)

	var count atomic.Int32
	prev := planExecRebuild
	planExecRebuild = func(spec *rebuildSpec, op *opRun) error {
		count.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the race window
		op.setResult(&ops.Result{Version: 99})
		return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{App: spec.Name, Version: 99}))
	}
	t.Cleanup(func() { planExecRebuild = prev; machine.LeaveJSON() })

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
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
	// In-process applies serialize on the plan-apply mutex: the first
	// executes, the others observe status=applied and replay the recorded
	// correlation. Every caller either succeeds with the SAME correlation or
	// fails closed — none may execute again.
	loaded, err := plans.Load(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != plans.StatusApplied || loaded.Correlation == nil {
		t.Fatalf("applied plan must carry its correlation: %+v", loaded.Correlation)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("apply #%d failed unexpectedly: %v", i, err)
		}
	}
}

func TestPlanApply_RestartLedgerReplay(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)

	count := withExecutionSeam(t)

	// Simulate a crash mid-application: begin the ledger entry exactly as
	// applyPlan would, then "restart" without completing it.
	if _, _, err := ops.BeginKey(planRequestKey(plan.PlanID), plan.PlanHash, ops.KindRebuild, plan.Action.Application); err != nil {
		t.Fatalf("BeginKey: %v", err)
	}

	// Fresh "process": the plan is still status=created, but the durable
	// ledger holds an in-progress entry for its key. The application must
	// fail closed with the indeterminate result — never re-execute.
	if err := applyPlan(plan.PlanID); err == nil {
		t.Fatal("interrupted application must fail closed, not execute again")
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("restart must never re-execute: seam ran %d time(s)", n)
	}
}

// TestPlanApply_FailedPlanIsSingleUseRegardlessOfLedger proves Task 4/10: a
// plan whose first apply fails is single-use. The first apply surfaces the real
// execution error; every retry is refused with a plan-appropriate PLAN_STALE
// ("already ran; create a new plan"), and that refusal is driven by the plan's
// own durable status — so it is IDENTICAL whether or not the request-key ledger
// entry still exists (here: deleted to simulate eviction). It never flips from
// "replay" to "re-execute".
func TestPlanApply_FailedPlanIsSingleUseRegardlessOfLedger(t *testing.T) {
	dir := planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)

	var count atomic.Int32
	prev := planExecRebuild
	planExecRebuild = func(spec *rebuildSpec, op *opRun) error {
		count.Add(1)
		return phelixerr.New(phelixerr.CodeDeployFailed, "boom")
	}
	t.Cleanup(func() { planExecRebuild = prev; machine.LeaveJSON() })

	// First apply: the real execution error surfaces, and the plan becomes
	// terminally failed.
	if err := applyPlan(plan.PlanID); !phelixerr.IsCode(err, phelixerr.CodeDeployFailed) {
		t.Fatalf("first apply = %v, want DEPLOY_FAILED", err)
	}
	if count.Load() != 1 {
		t.Fatalf("first apply exec count = %d, want 1", count.Load())
	}
	if p, _ := plans.Load(plan.PlanID); p.Status != plans.StatusFailed {
		t.Fatalf("plan status after failed apply = %q, want failed", p.Status)
	}

	// (a) retry: refused as single-use, no re-execution.
	if err := applyPlan(plan.PlanID); !phelixerr.IsCode(err, phelixerr.CodePlanStale) {
		t.Fatalf("retry (a) = %v, want PLAN_STALE", err)
	}
	if count.Load() != 1 {
		t.Fatalf("retry (a) re-executed: count=%d", count.Load())
	}

	// (b) retry after the ledger entry is gone (eviction simulated by deleting
	// the ledger file): identical refusal, still no re-execution.
	if err := os.Remove(filepath.Join(dir, "ops", "request-key-ledger.json")); err != nil {
		t.Fatalf("remove ledger: %v", err)
	}
	if err := applyPlan(plan.PlanID); !phelixerr.IsCode(err, phelixerr.CodePlanStale) {
		t.Fatalf("retry (b) after eviction = %v, want PLAN_STALE (identical)", err)
	}
	if count.Load() != 1 {
		t.Fatalf("retry (b) re-executed after eviction: count=%d", count.Load())
	}
}

// TestPlanApply_IndeterminateReplayReconcilesToFailed proves the crash path of
// Task 4: when the ledger replays an indeterminate/failed outcome for a plan
// still marked created (its process crashed mid-apply), the apply reconciles
// the plan to failed WITHOUT executing, so the next apply is gated by the
// durable status and never re-executes even if the ledger entry is later gone.
func TestPlanApply_IndeterminateReplayReconcilesToFailed(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)
	plan := manualRebuildPlan(t, info.Name, info.ID, nil)

	// Seed a COMPLETE failure (indeterminate-style) ledger entry for the plan's
	// derived key, as a crashed prior apply would have left after recovery.
	key := planRequestKey(plan.PlanID)
	fp := ops.Fingerprint(ops.KindRebuild, plan.Action.Application, map[string]string{"plan_hash": plan.PlanHash})
	if _, _, err := ops.BeginKey(key, fp, ops.KindRebuild, plan.Action.Application); err != nil {
		t.Fatalf("seed BeginKey: %v", err)
	}
	failEnv, _ := machine.MarshalEnvelope(machine.Failure(phelixerr.New(phelixerr.CodeUnavailable, "indeterminate"), 1, ""))
	if err := ops.CompleteKeyData(key, "", failEnv); err != nil {
		t.Fatalf("seed CompleteKeyData: %v", err)
	}

	count := withExecutionSeam(t)

	// Apply: the ledger replays (no execution), and the plan is reconciled to
	// failed.
	if err := applyPlan(plan.PlanID); err != nil {
		t.Fatalf("apply over replayed failure = %v, want nil (deterministic replay)", err)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("replay must not execute: seam ran %d time(s)", n)
	}
	if p, _ := plans.Load(plan.PlanID); p.Status != plans.StatusFailed {
		t.Fatalf("plan not reconciled: status = %q, want failed", p.Status)
	}

	// The next apply is now gated by the durable status.
	if err := applyPlan(plan.PlanID); !phelixerr.IsCode(err, phelixerr.CodePlanStale) {
		t.Fatalf("post-reconcile retry = %v, want PLAN_STALE", err)
	}
	if n := count.Load(); n != 0 {
		t.Fatalf("post-reconcile retry executed: count=%d", n)
	}
}

func TestPlanStale_ExitCodeIsValidation(t *testing.T) {
	for _, code := range []phelixerr.Code{
		phelixerr.CodePlanStale, phelixerr.CodePlanCorrupt, phelixerr.CodePlanHashMismatch,
		phelixerr.CodePlanInvalid, phelixerr.CodePlanCapabilityMissing,
	} {
		if got := ExitCodeFor(phelixerr.New(code, "x")); got != ExitValidation {
			t.Fatalf("%s exit = %d, want %d (validation, not execution failure)", code, got, ExitValidation)
		}
	}
}

// TestPlanCLI_EndToEnd runs the full machine contract as a subprocess with an
// isolated HOME (mirroring cli_integration_test.go): create → show → stale
// apply (zero mutation) → fresh plan. Staleness is induced by recreating the
// app with a different ID, so the rebuild path never runs and no real deploy
// happens in the test.
func TestPlanCLI_EndToEnd(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed one managed app. Directory points at a real, buildable project so
	// plan creation can resolve it; the stale path means it never deploys.
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "main.go"),
		[]byte("package main\n\nimport \"os\"\n\nfunc main() { _ = os.Getenv(\"PORT\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	apps := map[string]map[string]any{
		"11111111-1111-1111-1111-111111111111": {
			"id": "11111111-1111-1111-1111-111111111111", "name": "demo",
			"directory": projectDir, "language": "go", "port": 8080, "status": "stopped",
		},
	}
	blob, _ := json.Marshal(apps)
	if err := os.WriteFile(filepath.Join(home, ".phelix", "apps.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runPhelixInHome(t, home, "plan", "create", "rebuild", "demo", "--json")
	if code != 0 {
		t.Fatalf("plan create exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var env struct {
		SchemaVersion string `json:"schema_version"`
		Result        *struct {
			PlanID   string `json:"plan_id"`
			PlanHash string `json:"plan_hash"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("create stdout is not one envelope: %v\n%s", err, stdout)
	}
	planID := env.Result.PlanID
	if planID == "" {
		t.Fatal("no plan id in create response")
	}

	// Show: fresh plan is applicable, hash matches.
	code, stdout, _ = runPhelixInHome(t, home, "plan", "show", planID, "--json")
	if code != 0 {
		t.Fatalf("plan show exit=%d stdout=%s", code, stdout)
	}
	var showEnv struct {
		Result *struct {
			Applicability struct {
				State string `json:"state"`
			} `json:"applicability"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &showEnv); err != nil {
		t.Fatalf("show stdout is not one envelope: %v", err)
	}
	if showEnv.Result.Applicability.State != "applicable" {
		t.Fatalf("fresh plan applicability = %q, want applicable", showEnv.Result.Applicability.State)
	}

	// Recreate the app under a different ID: app identity drifted.
	apps["22222222-2222-2222-2222-222222222222"] = apps["11111111-1111-1111-1111-111111111111"]
	delete(apps, "11111111-1111-1111-1111-111111111111")
	blob, _ = json.Marshal(apps)
	if err := os.WriteFile(filepath.Join(home, ".phelix", "apps.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	// Apply the stale plan: PLAN_STALE, exit 3, one JSON document, no mutation.
	code, stdout, stderr = runPhelixInHome(t, home, "plan", "apply", planID, "--json")
	if code != ExitValidation {
		t.Fatalf("stale apply exit=%d, want %d (stderr=%s)", code, ExitValidation, stderr)
	}
	var staleEnv struct {
		Error *struct {
			Code     string `json:"code"`
			ExitCode int    `json:"exit_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &staleEnv); err != nil {
		t.Fatalf("stale apply stdout is not one envelope: %v\n%s", err, stdout)
	}
	if staleEnv.Error == nil || staleEnv.Error.Code != "PLAN_STALE" || staleEnv.Error.ExitCode != ExitValidation {
		t.Fatalf("stale error body wrong: %+v", staleEnv.Error)
	}

	// A fresh plan for the recreated app is applicable again — no silent
	// replanning happened behind the agent's back.
	code, stdout, _ = runPhelixInHome(t, home, "plan", "create", "rebuild", "demo", "--json")
	if code != 0 {
		t.Fatalf("recreate exit=%d stdout=%s", code, stdout)
	}
	var fresh struct {
		Result *struct {
			PlanID        string `json:"plan_id"`
			Applicability struct {
				State string `json:"state"`
			} `json:"applicability"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &fresh); err != nil {
		t.Fatalf("recreate stdout is not one envelope: %v", err)
	}
	if fresh.Result.PlanID == planID {
		t.Fatal("new plan must have a new id")
	}
	if fresh.Result.Applicability.State != "applicable" {
		t.Fatalf("fresh plan after drift = %q, want applicable", fresh.Result.Applicability.State)
	}
}

// TestPlanApplicability_SourceDriftUnprotectedForNonGit proves Task 9: a
// rebuild plan with no source_commit precondition (non-git source) reports
// source_drift_unprotected, while a git-backed plan does not.
func TestPlanApplicability_SourceDriftUnprotectedForNonGit(t *testing.T) {
	planTestDataDir(t)
	info := withAppStore(t)

	nonGit := manualRebuildPlan(t, info.Name, info.ID, nil)
	if a := computeApplicability(nonGit); !a.SourceDriftUnprotected {
		t.Fatal("non-git rebuild plan must report source_drift_unprotected")
	}

	gitBacked := manualRebuildPlan(t, info.Name, info.ID, []plans.Precondition{
		{Type: plans.PreconditionSourceCommit, Expected: "abc123", Source: "git"},
	})
	if a := computeApplicability(gitBacked); a.SourceDriftUnprotected {
		t.Fatal("git-backed plan must NOT report source_drift_unprotected")
	}
}
