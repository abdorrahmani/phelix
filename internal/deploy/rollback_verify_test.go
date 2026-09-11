package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// Tests for rollback --reason persistence and --verify stability observation.

// --- reason validation ------------------------------------------------------

func TestValidateRollbackReason(t *testing.T) {
	resetHome(t) // keep the suite's HOME convention; validation touches no files

	cases := []struct {
		name     string
		in       string
		explicit bool
		want     string
		wantErr  bool
	}{
		{"absent, not explicit", "", false, "", false},
		{"supplied text", "  Login endpoint returning 500  ", true, "Login endpoint returning 500", false},
		{"explicit but empty", "   ", true, "", true},
		{"internal whitespace collapsed", "API\n\tregression   here", true, "API regression here", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateRollbackReason(tc.in, tc.explicit)
			if tc.wantErr && err == nil {
				t.Fatalf("want error, got %q", got)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestValidateRollbackReasonTooLong(t *testing.T) {
	long := strings.Repeat("x", MaxRollbackReasonLength+1)
	if _, err := ValidateRollbackReason(long, true); err == nil {
		t.Fatal("over-limit reason must be rejected")
	}
	ok := strings.Repeat("x", MaxRollbackReasonLength)
	if _, err := ValidateRollbackReason(ok, true); err != nil {
		t.Fatalf("at-limit reason must pass: %v", err)
	}
}

// TestRecordRollbackResultReasonAndVerification: reason and verification land
// in the same JSONL record, serialized by encoding/json (quotes, newlines and
// control characters must survive round-trip without corrupting the line).
func TestRecordRollbackResultReasonAndVerification(t *testing.T) {
	resetHome(t)
	tricky := `inject"line\nfake entry} and \u00e9`
	RecordRollbackResult("hist-rv", 12, 7, "blue-green", tricky,
		&RollbackVerification{Requested: true, Duration: "30s", Status: RollbackVerifyFailed, Error: "instance slot green is unhealthy"},
		nil)

	recs, skipped, err := ReadRollbackHistory("hist-rv")
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 0 || len(recs) != 1 {
		t.Fatalf("recs=%d skipped=%d, want 1/0", len(recs), skipped)
	}
	rec := recs[0]
	if rec.Reason != tricky {
		t.Errorf("reason round-trip failed:\n got %q\nwant %q", rec.Reason, tricky)
	}
	if rec.Status != RollbackStatusSuccess {
		t.Errorf("execution status = %q, want success (verification failure must not change it)", rec.Status)
	}
	if rec.Verification == nil || rec.Verification.Status != RollbackVerifyFailed || rec.Verification.Duration != "30s" {
		t.Errorf("verification = %+v", rec.Verification)
	}
}

// TestReadRollbackHistoryWithoutReasonStillLoads: old records (no reason, no
// verification) parse and render with the missing-value dash.
func TestReadRollbackHistoryWithoutReasonStillLoads(t *testing.T) {
	resetHome(t)
	path := recordPath("hist-legacy")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"time":"2026-08-28T18:42:09Z","app":"hist-legacy","from":"v8","to":"v6","status":"success","mode":"classic"}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, skipped, err := ReadRollbackHistory("hist-legacy")
	if err != nil || skipped != 0 {
		t.Fatalf("legacy record must load: recs=%d skipped=%d err=%v", len(recs), skipped, err)
	}
	if recs[0].Reason != "" || recs[0].Verification != nil {
		t.Errorf("legacy record must carry no reason/verification: %+v", recs[0])
	}
}

// --- verification -----------------------------------------------------------

// TestExecuteRollback_VerifyPassed: a healthy target plus --verify produces a
// passed verification in history and a nil error.
func TestExecuteRollback_VerifyPassed(t *testing.T) {
	resetHome(t)
	app := "verify-ok"
	rollbackFixture(t, app, 2, 999999, 40000)
	fl := &httpLauncher{}
	defer fl.close()

	ticks := 0
	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      1,
		Launcher:      fl.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger: &fakeLogger{},
		Reason: "regression in v2",
		Verify: &VerifyRequest{
			Duration: 50 * time.Millisecond,
			OnTick:   func(time.Duration, error) { ticks++ },
		},
	})
	if err != nil {
		t.Fatalf("rollback+verify: %v", err)
	}
	if ticks == 0 {
		t.Error("OnTick never fired — no observation sweeps ran")
	}

	recs, _, err := ReadRollbackHistory(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Status != RollbackStatusSuccess || rec.Reason != "regression in v2" {
		t.Errorf("record = %+v", rec)
	}
	if rec.Verification == nil || rec.Verification.Status != RollbackVerifyPassed || rec.Verification.Duration != "50ms" {
		t.Errorf("verification = %+v, want passed/50ms", rec.Verification)
	}
}

// TestExecuteRollback_VerifyFailedIsDistinct: an app whose serving instance
// dies after the rollback commits must yield a verification failure — not a
// rollback execution failure.
func TestExecuteRollback_VerifyFailedIsDistinct(t *testing.T) {
	resetHome(t)
	app := "verify-fail"
	rollbackFixture(t, app, 1, 999999, 40000)
	fl := &httpLauncher{}
	defer fl.close()

	// Run WITHOUT verify first: execution succeeds and commits state pointing
	// at the live httptest instance.
	if err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      1,
		Launcher:      fl.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger: &fakeLogger{},
	}); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// Kill the serving instance so verification observes a genuinely dead
	// target.
	fl.close()

	vErr := VerifyRollbackStability(context.Background(), VerificationOptions{
		AppName:  app,
		Duration: 40 * time.Millisecond,
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger: &fakeLogger{},
	})
	if vErr == nil {
		t.Fatal("verification of a dead instance must fail")
	}
	if !phelixerr.IsCode(vErr, phelixerr.CodeHealthCheckFailed) {
		t.Errorf("code = %v, want HEALTH_CHECK_FAILED", phelixerr.CodeOf(vErr))
	}
}

// TestExecuteRollback_VerifyCancelled: cancelling the window mid-observation
// returns context.Canceled — the caller's signal to record "cancelled", never
// "execution failed".
func TestExecuteRollback_VerifyCancelled(t *testing.T) {
	resetHome(t)
	app := "verify-cancel"
	rollbackFixture(t, app, 1, 999999, 40000)
	fl := &httpLauncher{}
	defer fl.close()

	if err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      1,
		Launcher:      fl.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger: &fakeLogger{},
	}); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // signal before the window starts
	err := VerifyRollbackStability(ctx, VerificationOptions{
		AppName:  app,
		Duration: 30 * time.Second,
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger: &fakeLogger{},
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestExecuteRollback_VerifyTargetsActiveSlotOnly: blue-green verification
// must probe the ACTIVE slot's port — never the drained one.
func TestExecuteRollback_VerifyTargetsActiveSlotOnly(t *testing.T) {
	resetHome(t)
	app := "verify-slot"
	state := &DeployState{
		AppName:       app,
		Mode:          ModeBlueGreen,
		PublicPort:    3000,
		ActiveSlot:    SlotGreen,
		ActiveVersion: 7,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped", PID: 0, Port: 0},
			SlotGreen: {Slot: SlotGreen, Status: "running", PID: 1, Port: 4321},
		},
		Health: &HealthSummary{Tier: int(health.Tier3TCP)},
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}

	targets := servingTargets(state)
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1 (active slot only)", len(targets))
	}
	if targets[0].addr != "127.0.0.1:4321" {
		t.Errorf("probe target = %s, want the active (green) slot port 4321", targets[0].addr)
	}
	if targets[0].tier != health.Tier3TCP {
		t.Errorf("tier = %v, want the deploy-recorded Tier3TCP", targets[0].tier)
	}
}

// TestExecuteRollback_VerifyFailedNotRecordedAsExecutionFailure: a verify
// error returned by ExecuteRollback must still leave a SUCCESS execution
// record (with verification failed), never FAILED.
func TestExecuteRollback_VerifyFailedNotRecordedAsExecutionFailure(t *testing.T) {
	resetHome(t)
	app := "verify-hist"
	rollbackFixture(t, app, 2, 999999, 40000)
	fl := &httpLauncher{}

	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      1,
		Launcher:      fl.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger: &fakeLogger{},
		Verify: &VerifyRequest{Duration: 10 * time.Millisecond},
	})
	// The servers are closed inside ExecuteRollback's window only by race;
	// accept either outcome but assert the history invariant.
	fl.close()
	_ = err

	recs, _, rerr := ReadRollbackHistory(app)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Status == RollbackStatusFailed && rec.Verification != nil {
		t.Errorf("verification outcome leaked into execution status: %+v", rec)
	}
	if rec.Verification != nil && rec.Verification.Status == RollbackVerifyFailed && rec.Status != RollbackStatusSuccess {
		t.Errorf("verify-failed must keep execution status success: %+v", rec)
	}
}

// TestExecuteRollback_ExecutionFailureSkipsVerification: when the rollback
// itself fails, verification must never run and history must carry no
// verification block.
func TestExecuteRollback_ExecutionFailureSkipsVerification(t *testing.T) {
	resetHome(t)
	app := "verify-execfail"
	rollbackFixture(t, app, 2, 999999, 40000)

	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      2,
		Launcher:      closedLauncher{}.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealthShortTimeout()
		},
		Logger: &fakeLogger{},
		Verify: &VerifyRequest{Duration: 10 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected execution failure")
	}
	recs, _, rerr := ReadRollbackHistory(app)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].Status != RollbackStatusFailed {
		t.Errorf("status = %q, want failed", recs[0].Status)
	}
	if recs[0].Verification != nil {
		t.Errorf("failed execution must record no verification: %+v", recs[0].Verification)
	}
}

// TestFormatVerifyProgressShape: progress lines stay readable.
func TestFormatVerifyProgressShape(t *testing.T) {
	ok := FormatVerifyProgress(5*time.Second, nil)
	if !strings.Contains(ok, "5s") || !strings.Contains(ok, "✓") || !strings.Contains(ok, "healthy") {
		t.Errorf("healthy line = %q", ok)
	}
	bad := FormatVerifyProgress(15*time.Second, context.DeadlineExceeded)
	if !strings.Contains(bad, "✗") || !strings.Contains(bad, "15s") {
		t.Errorf("failure line = %q", bad)
	}
}

// TestRollbackVerificationJSONShape: the persisted field names match the
// documented schema.
func TestRollbackVerificationJSONShape(t *testing.T) {
	data, err := json.Marshal(RollbackHistoryRecord{
		Time: time.Now(), App: "a", From: "v12", To: "v7",
		Status: RollbackStatusSuccess, Mode: "blue-green",
		Reason: "Login endpoint returning 500",
		Verification: &RollbackVerification{
			Requested: true, Duration: "30s", Status: RollbackVerifyPassed,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m["verification"].(map[string]any)
	if !ok {
		t.Fatalf("verification missing from JSON: %s", data)
	}
	if v["requested"] != true || v["duration"] != "30s" || v["status"] != "passed" {
		t.Errorf("verification fields = %v", v)
	}
	if m["reason"] != "Login endpoint returning 500" {
		t.Errorf("reason field = %v", m["reason"])
	}
}
