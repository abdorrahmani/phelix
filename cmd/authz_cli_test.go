package cmd

// authz_cli_test.go — the Phase 4 machine contract end-to-end, as a real
// subprocess against an isolated $HOME (mirroring cli_integration_test.go and
// the Phase 3 plan end-to-end test).
//
// This is the automated form of the agent workflow: inspect → plan → refuse →
// approve → verify → prove non-transferability → prove staleness. It
// deliberately never reaches a real deployment: the mutation-safety suite in
// authz_test.go proves execution counts with a seam, and an end-to-end test
// that built and started a server would be a flaky way to re-prove it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authzEnvelope is the subset of the Phase 1 envelope these assertions need.
type authzEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	Status        string          `json:"status"`
	OperationID   string          `json:"operation_id"`
	Result        json.RawMessage `json:"result"`
	Error         *struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		ExitCode  int    `json:"exit_code"`
		Retryable *bool  `json:"retryable"`
	} `json:"error"`
}

// decodeOneEnvelope asserts the stdout of a --json command is exactly one
// envelope at schema 1 — no banners, no second document.
func decodeOneEnvelope(t *testing.T, stdout string) authzEnvelope {
	t.Helper()
	var env authzEnvelope
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout carried more than one JSON document:\n%s", stdout)
	}
	if env.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want \"1\"", env.SchemaVersion)
	}
	return env
}

func unmarshalResult(t *testing.T, env authzEnvelope, out any) {
	t.Helper()
	if len(env.Result) == 0 {
		t.Fatal("envelope has no result")
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		t.Fatalf("result does not decode: %v\n%s", err, env.Result)
	}
}

// seedAuthzHome prepares an isolated HOME with one managed app pointing at a
// real, buildable Go project.
func seedAuthzHome(t *testing.T) (home string, appID string) {
	t.Helper()
	home = t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".phelix"), 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"),
		[]byte("module demo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "main.go"),
		[]byte("package main\n\nimport \"os\"\n\nfunc main() { _ = os.Getenv(\"PORT\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appID = "11111111-1111-1111-1111-111111111111"
	apps := map[string]map[string]any{
		appID: {
			"id": appID, "name": "demo", "directory": projectDir,
			"language": "go", "port": 8080, "status": "stopped",
		},
	}
	blob, _ := json.Marshal(apps)
	if err := os.WriteFile(filepath.Join(home, ".phelix", "apps.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	return home, appID
}

// writeHomePolicy installs a host authorization policy in an isolated HOME.
func writeHomePolicy(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".phelix", "authz")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// createPlan creates a rebuild plan through the machine contract and returns
// its id and hash.
func createPlan(t *testing.T, home string, extra ...string) (planID, planHash string) {
	t.Helper()
	args := append([]string{"plan", "create", "rebuild", "demo", "--json"}, extra...)
	code, stdout, stderr := runPhelixInHome(t, home, args...)
	if code != 0 {
		t.Fatalf("plan create exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	env := decodeOneEnvelope(t, stdout)
	var res struct {
		PlanID   string `json:"plan_id"`
		PlanHash string `json:"plan_hash"`
	}
	unmarshalResult(t, env, &res)
	if res.PlanID == "" || res.PlanHash == "" {
		t.Fatalf("plan create did not return an identity: %s", env.Result)
	}
	return res.PlanID, res.PlanHash
}

// TestAuthzCLI_EndToEnd walks the full machine-facing workflow.
func TestAuthzCLI_EndToEnd(t *testing.T) {
	home, appID := seedAuthzHome(t)

	// --- An unconfigured host reports the legacy posture ------------------
	code, stdout, stderr := runPhelixInHome(t, home, "authz", "status", "--json")
	if code != 0 {
		t.Fatalf("authz status exit=%d stderr=%s", code, stderr)
	}
	var status struct {
		Mode         string `json:"mode"`
		Enforced     bool   `json:"enforced"`
		PolicyLoaded bool   `json:"policy_loaded"`
		Actor        struct {
			Type          string  `json:"type"`
			ID            *string `json:"id"`
			Authenticated bool    `json:"authenticated"`
		} `json:"actor"`
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &status)
	if status.Mode != "legacy_local" || status.Enforced {
		t.Fatalf("an unconfigured host must report legacy_local: %+v", status)
	}
	// The Phase 1 actor block is unchanged: no fabricated identity.
	if status.Actor.Type != "cli" || status.Actor.ID != nil || status.Actor.Authenticated {
		t.Fatalf("actor must stay the unauthenticated CLI actor: %+v", status.Actor)
	}

	// --- Step 1-3: plan the mutation --------------------------------------
	planID, planHash := createPlan(t, home)

	code, stdout, _ = runPhelixInHome(t, home, "plan", "show", planID, "--json")
	if code != 0 {
		t.Fatalf("plan show exit=%d stdout=%s", code, stdout)
	}
	var show struct {
		PlanID        string   `json:"plan_id"`
		PlanHash      string   `json:"plan_hash"`
		Capabilities  []string `json:"capabilities"`
		Applicability struct {
			State string `json:"state"`
		} `json:"applicability"`
		Action struct {
			Type        string `json:"type"`
			Application string `json:"application"`
		} `json:"action"`
		Preconditions []struct {
			Type string `json:"type"`
		} `json:"preconditions"`
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &show)
	if show.PlanHash != planHash || show.Applicability.State != "applicable" {
		t.Fatalf("plan show disagrees with create: %+v", show)
	}
	if show.Action.Type != "rebuild" || show.Action.Application != "demo" ||
		len(show.Preconditions) == 0 || len(show.Capabilities) == 0 {
		t.Fatalf("plan show is missing the fields an agent drives on: %+v", show)
	}

	// --- Enforce authorization with a per-plan approval requirement -------
	writeHomePolicy(t, home, `{
  "schema_version": "1",
  "mode": "enforced",
  "rules": [
    {"actor": {"type": "cli", "authenticated": false},
     "action": "rebuild", "target": "demo", "effect": "allow", "require_approval": true}
  ]
}`)

	code, stdout, stderr = runPhelixInHome(t, home, "authz", "status", "--json")
	if code != 0 {
		t.Fatalf("authz status exit=%d stderr=%s", code, stderr)
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &status)
	if status.Mode != "enforced" || !status.Enforced || !status.PolicyLoaded {
		t.Fatalf("the host must report enforced authorization: %+v", status)
	}

	// --- Step 4: execution without authorization is refused ---------------
	assertApplyRefused(t, home, planID, "APPROVAL_REQUIRED", false)

	// `authz check` agrees, without mutating anything.
	code, stdout, _ = runPhelixInHome(t, home, "authz", "check", planID, "--json")
	if code != 0 {
		t.Fatalf("authz check exit=%d stdout=%s", code, stdout)
	}
	var check struct {
		PlanID   string `json:"plan_id"`
		PlanHash string `json:"plan_hash"`
		Decision string `json:"decision"`
		Code     string `json:"code"`
		Mode     string `json:"mode"`
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &check)
	if check.Decision != "approval_required" || check.Code != "APPROVAL_REQUIRED" {
		t.Fatalf("authz check disagrees with apply: %+v", check)
	}
	if check.PlanID != planID || check.PlanHash != planHash {
		t.Fatalf("authz check lost the plan binding: %+v", check)
	}

	// --- Step 5: approving the WRONG content is refused -------------------
	code, stdout, _ = runPhelixInHome(t, home, "authz", "approve", planID,
		"--expect-hash", "sha256:deadbeef", "--json")
	if code != ExitPermission {
		t.Fatalf("approving a mismatched hash exit=%d, want %d (stdout=%s)", code, ExitPermission, stdout)
	}
	env := decodeOneEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "APPROVAL_INVALID" {
		t.Fatalf("expected APPROVAL_INVALID, got %+v", env.Error)
	}

	// Approving the exact content the agent inspected succeeds.
	code, stdout, stderr = runPhelixInHome(t, home, "authz", "approve", planID,
		"--expect-hash", planHash, "--json")
	if code != 0 {
		t.Fatalf("authz approve exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var approval struct {
		ApprovalID     string `json:"approval_id"`
		PlanID         string `json:"plan_id"`
		PlanHash       string `json:"plan_hash"`
		Action         string `json:"action"`
		Target         string `json:"target"`
		Decision       string `json:"decision"`
		ApproverSource string `json:"approver_source"`
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &approval)
	if approval.PlanID != planID || approval.PlanHash != planHash {
		t.Fatalf("the approval is not bound to the plan: %+v", approval)
	}
	if approval.Action != "rebuild" || approval.Target != "demo" || approval.Decision != "approved" {
		t.Fatalf("approval scope is wrong: %+v", approval)
	}
	if approval.ApproverSource != "local_host" {
		t.Fatalf("approver_source = %q, want local_host — Phase 4 must not claim a verified identity",
			approval.ApproverSource)
	}

	// Re-approving is refused: approvals are immutable.
	code, stdout, _ = runPhelixInHome(t, home, "authz", "approve", planID, "--json")
	if code == 0 {
		t.Fatal("re-approving a plan must not silently replace the approval")
	}
	if env := decodeOneEnvelope(t, stdout); env.Error == nil || env.Error.Code != "ALREADY_EXISTS" {
		t.Fatalf("expected ALREADY_EXISTS, got %+v", env.Error)
	}

	// --- The boundary now allows this exact plan --------------------------
	code, stdout, _ = runPhelixInHome(t, home, "authz", "check", planID, "--json")
	if code != 0 {
		t.Fatalf("authz check exit=%d stdout=%s", code, stdout)
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &check)
	if check.Decision != "allow" || check.Code != "AUTHZ_ALLOWED" {
		t.Fatalf("an approved plan must be allowed: %+v", check)
	}

	// --- Step 7: the approval is not transferable to another plan ---------
	// A second plan for the same action and target, differing only in its
	// build tag, is a different execution and is NOT covered.
	otherID, otherHash := createPlan(t, home, "--tag", "variant-b")
	if otherID == planID || otherHash == planHash {
		t.Fatal("the second plan must have its own identity")
	}
	assertApplyRefused(t, home, otherID, "APPROVAL_REQUIRED", false)

	code, stdout, _ = runPhelixInHome(t, home, "authz", "check", otherID, "--json")
	if code != 0 {
		t.Fatalf("authz check exit=%d", code)
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &check)
	if check.Decision == "allow" {
		t.Fatal("plan A's approval must not authorize plan B")
	}

	// --- Revocation works, and is visible immediately ---------------------
	code, stdout, _ = runPhelixInHome(t, home, "authz", "revoke", planID, "--json")
	if code != 0 {
		t.Fatalf("authz revoke exit=%d stdout=%s", code, stdout)
	}
	assertApplyRefused(t, home, planID, "APPROVAL_REQUIRED", false)

	// Re-approve for the staleness step below.
	if code, stdout, _ = runPhelixInHome(t, home, "authz", "approve", planID,
		"--expect-hash", planHash, "--json"); code != 0 {
		t.Fatalf("re-approve after revoke exit=%d stdout=%s", code, stdout)
	}

	// --- Step 8: plan validity is independent of authorization -----------
	// Recreate the app under a different ID: the plan is now stale. An
	// approved, allowed plan must STILL refuse to execute.
	appsPath := filepath.Join(home, ".phelix", "apps.json")
	raw, err := os.ReadFile(appsPath)
	if err != nil {
		t.Fatal(err)
	}
	var apps map[string]map[string]any
	if err := json.Unmarshal(raw, &apps); err != nil {
		t.Fatal(err)
	}
	newID := "22222222-2222-2222-2222-222222222222"
	apps[newID] = apps[appID]
	apps[newID]["id"] = newID
	delete(apps, appID)
	blob, _ := json.Marshal(apps)
	if err := os.WriteFile(appsPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	assertApplyRefused(t, home, planID, "PLAN_STALE", true)

	// --- Locking the host down denies even an approved plan --------------
	writeHomePolicy(t, home, `{"schema_version":"1","mode":"enforced","rules":[]}`)
	freshID, freshHash := createPlan(t, home)
	if code, stdout, _ = runPhelixInHome(t, home, "authz", "approve", freshID,
		"--expect-hash", freshHash, "--json"); code != 0 {
		t.Fatalf("approve exit=%d stdout=%s", code, stdout)
	}
	assertApplyRefused(t, home, freshID, "AUTHZ_DENIED", false)

	// --- A broken policy fails closed, it does not fall back -------------
	writeHomePolicy(t, home, `{"schema_version":"1","mode":"enforced","rules":[{"bad":true}]}`)
	assertApplyRefused(t, home, freshID, "AUTHZ_INVALID", false)

	code, stdout, _ = runPhelixInHome(t, home, "authz", "status", "--json")
	if code != 0 {
		t.Fatalf("authz status must stay readable with a broken policy, exit=%d", code)
	}
	var broken struct {
		Mode        string `json:"mode"`
		Enforced    bool   `json:"enforced"`
		PolicyError *struct {
			Code string `json:"code"`
		} `json:"policy_error"`
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &broken)
	if broken.PolicyError == nil || broken.PolicyError.Code != "AUTHZ_INVALID" || !broken.Enforced {
		t.Fatalf("a broken policy must be reported as enforcing-and-failing-closed: %+v", broken)
	}

	// --- Direct, planless execution is denied under enforcement ----------
	// This is the bypass that matters: skipping the plan must not skip the
	// boundary. (The policy is still broken here, so the refusal is the
	// fail-closed one; a valid enforced policy denies it as planless.)
	writeHomePolicy(t, home, `{
  "schema_version": "1",
  "mode": "enforced",
  "rules": [
    {"actor": {"type": "*", "authenticated": false},
     "action": "*", "target": "*", "effect": "allow"}
  ]
}`)
	code, stdout, stderr = runPhelixInHome(t, home, "rebuild", "demo", "--json")
	if code != ExitPermission {
		t.Fatalf("planless rebuild exit=%d, want %d (stdout=%s stderr=%s)", code, ExitPermission, stdout, stderr)
	}
	env = decodeOneEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "AUTHZ_DENIED" {
		t.Fatalf("planless rebuild must be AUTHZ_DENIED, got %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "plan") {
		t.Fatalf("the denial must tell the caller to use a plan: %q", env.Error.Message)
	}
	// Diagnostics stay on stderr; stdout is the envelope alone.
	if strings.Contains(stdout, "Error:") {
		t.Fatalf("human diagnostics leaked onto stdout:\n%s", stdout)
	}
	if stderr == "" {
		t.Fatal("the human error block must still be rendered on stderr")
	}
}

// TestAuthzCLI_MachineContractPurity pins the Phase 1 stdout/stderr
// discipline for every new command: exactly one envelope at schema 1 on
// stdout, nothing else, on both the success and the failure path.
func TestAuthzCLI_MachineContractPurity(t *testing.T) {
	home, _ := seedAuthzHome(t)
	planID, planHash := createPlan(t, home)
	writeHomePolicy(t, home, `{
  "schema_version": "1",
  "mode": "enforced",
  "rules": [
    {"actor": {"type": "cli", "authenticated": false},
     "action": "rebuild", "target": "demo", "effect": "allow", "require_approval": true}
  ]
}`)

	cases := []struct {
		name     string
		args     []string
		wantExit int
	}{
		{"status", []string{"authz", "status", "--json"}, 0},
		{"check (approval required)", []string{"authz", "check", planID, "--json"}, 0},
		{"approve (hash mismatch)", []string{"authz", "approve", planID, "--expect-hash", "sha256:nope", "--json"}, ExitPermission},
		{"approve", []string{"authz", "approve", planID, "--expect-hash", planHash, "--json"}, 0},
		{"check (allowed)", []string{"authz", "check", planID, "--json"}, 0},
		{"revoke", []string{"authz", "revoke", planID, "--json"}, 0},
		{"revoke (nothing to revoke)", []string{"authz", "revoke", planID, "--json"}, ExitNotFound},
		{"check (unknown plan)", []string{"authz", "check", "pln_0000000000000000", "--json"}, ExitNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runPhelixInHome(t, home, tc.args...)
			if code != tc.wantExit {
				t.Fatalf("exit = %d, want %d (stdout=%s stderr=%s)", code, tc.wantExit, stdout, stderr)
			}
			env := decodeOneEnvelope(t, stdout)
			if tc.wantExit == 0 {
				if env.Status != "succeeded" || env.Error != nil {
					t.Fatalf("expected a success envelope, got %s", stdout)
				}
			} else {
				if env.Status != "failed" || env.Error == nil {
					t.Fatalf("expected a failure envelope, got %s", stdout)
				}
				if env.Error.ExitCode != tc.wantExit {
					t.Fatalf("error.exit_code = %d, want %d", env.Error.ExitCode, tc.wantExit)
				}
			}
			// Read-only authorization commands never fabricate an operation id.
			if env.OperationID != "" {
				t.Fatalf("a read-only authorization command must not report an operation id: %q", env.OperationID)
			}
			if strings.Contains(stdout, "Error:") || strings.Contains(stdout, "Mode:") {
				t.Fatalf("human output leaked onto stdout:\n%s", stdout)
			}
		})
	}
}

// assertApplyRefused applies a plan expecting a refusal, and verifies the
// whole failure contract: one envelope, the exact code, the right exit code,
// explicit retryability, and no operation left behind.
func assertApplyRefused(t *testing.T, home, planID, wantCode string, validation bool) {
	t.Helper()
	wantExit := ExitPermission
	if validation {
		wantExit = ExitValidation
	}
	code, stdout, stderr := runPhelixInHome(t, home, "plan", "apply", planID, "--json")
	if code != wantExit {
		t.Fatalf("apply %s: exit=%d, want %d (stdout=%s stderr=%s)", wantCode, code, wantExit, stdout, stderr)
	}
	env := decodeOneEnvelope(t, stdout)
	if env.Status != "failed" || env.Error == nil {
		t.Fatalf("apply %s: envelope is not a failure: %s", wantCode, stdout)
	}
	if env.Error.Code != wantCode {
		t.Fatalf("apply: code = %q, want %q (message: %s)", env.Error.Code, wantCode, env.Error.Message)
	}
	if env.Error.ExitCode != wantExit {
		t.Fatalf("apply %s: error.exit_code = %d, want %d", wantCode, env.Error.ExitCode, wantExit)
	}
	if env.Error.Retryable == nil {
		t.Fatalf("apply %s: retryable must be explicit", wantCode)
	}
	if *env.Error.Retryable {
		t.Fatalf("apply %s: a refusal an agent cannot fix by retrying must not be retryable", wantCode)
	}
	// Zero mutation: the plan never reaches applied, and no operation exists.
	code, stdout, _ = runPhelixInHome(t, home, "plan", "show", planID, "--json")
	if code != 0 {
		return // a stale/corrupt plan may itself fail to show; nothing applied either way
	}
	var show struct {
		Status      string `json:"status"`
		Correlation *struct {
			OperationID string `json:"operation_id"`
		} `json:"correlation"`
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &show)
	if show.Status == "applied" || show.Correlation != nil {
		t.Fatalf("a refused plan must not be applied or correlated: status=%q correlation=%+v",
			show.Status, show.Correlation)
	}
	code, stdout, _ = runPhelixInHome(t, home, "operation", "list", "--json")
	if code != 0 {
		t.Fatalf("operation list exit=%d", code)
	}
	var list struct {
		Count int `json:"count"`
	}
	unmarshalResult(t, decodeOneEnvelope(t, stdout), &list)
	if list.Count != 0 {
		t.Fatalf("MUTATION HAPPENED: a refused execution recorded %d operation(s)", list.Count)
	}
}
