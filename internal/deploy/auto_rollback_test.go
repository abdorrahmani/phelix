package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Tests for automatic rollback: last-known-good resolution, the no-op path
// when the failure never reached traffic, real recovery through
// ExecuteRollback, recovery failure reporting, history origin tagging and
// trigger classification.

// lgFixture prepares versions v1..v3 with the given promoted set and marks
// newVer current (or no current when 0). Binaries are copied from the test
// binary so VersionPaths succeeds. mode defaults to rolling.
func lgFixture(t *testing.T, app string, promoted map[int]bool, current int) {
	t.Helper()
	for v := 1; v <= 3; v++ {
		vdir, err := versionDir(app, v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			t.Fatal(err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vdir, "binary"), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 3; v++ {
		meta := VersionMeta{Version: v, BuiltAt: time.Now(), IsCurrent: v == current}
		if promoted[v] {
			now := time.Now().Add(-time.Duration(4-v) * time.Hour)
			meta.DeployedAt = &now
		}
		vf.Versions = append(vf.Versions, meta)
	}
	if err := saveVersions(app, vf); err != nil {
		t.Fatal(err)
	}
}

// TestLastKnownGoodVersion_PrefersPromoted: only versions that were actually
// promoted qualify, regardless of version number order.
func TestLastKnownGoodVersion_PrefersPromoted(t *testing.T) {
	resetHome(t)
	app := "lkg-promoted"
	lgFixture(t, app, map[int]bool{1: true, 2: true}, 2)

	got, err := LastKnownGoodVersion(app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Fatalf("LastKnownGoodVersion = v%d, want v2 (highest promoted)", got)
	}
}

// TestLastKnownGoodVersion_SkipsUnpromoted: a failed deploy (v3 recorded but
// never promoted) must never be chosen — the classic v10-good / v11-bad /
// v12-bad scenario mapped onto v1..v3.
func TestLastKnownGoodVersion_SkipsUnpromoted(t *testing.T) {
	resetHome(t)
	app := "lkg-unpromoted"
	lgFixture(t, app, map[int]bool{1: true, 2: true}, 2) // v3 exists but unpromoted

	got, err := LastKnownGoodVersion(app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Fatalf("LastKnownGoodVersion = v%d, want v2 (v3 was never promoted)", got)
	}
}

// TestLastKnownGoodVersion_SkipsMissingBinary: a promoted version whose
// artifacts were pruned cannot be rolled back to.
func TestLastKnownGoodVersion_SkipsMissingBinary(t *testing.T) {
	resetHome(t)
	app := "lkg-missing"
	lgFixture(t, app, map[int]bool{1: true, 3: true}, 3)
	if err := os.RemoveAll(filepath.Join(appDataDirMust(t, app), "builds", "v3")); err != nil {
		t.Fatal(err)
	}

	got, err := LastKnownGoodVersion(app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Fatalf("LastKnownGoodVersion = v%d, want v1 (v3 binary pruned)", got)
	}
}

func appDataDirMust(t *testing.T, app string) string {
	t.Helper()
	dir, err := appDataDir(app)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLastKnownGoodVersion_None: no promoted version at all → clear error,
// never a fabricated target.
func TestLastKnownGoodVersion_None(t *testing.T) {
	resetHome(t)
	app := "lkg-none"
	lgFixture(t, app, map[int]bool{}, 0)

	if _, err := LastKnownGoodVersion(app); err == nil {
		t.Fatal("want error when no version was ever promoted")
	}
}

// TestAutoRollbackTarget_RejectsFailedVersion: when the only known-good
// version IS the one that just failed, recovery must refuse rather than
// re-roll to the same version.
func TestAutoRollbackTarget_RejectsFailedVersion(t *testing.T) {
	resetHome(t)
	app := "lkg-same"
	lgFixture(t, app, map[int]bool{2: true}, 2)

	if _, err := AutoRollbackTarget(app, 2); err == nil {
		t.Fatal("want error when the only known-good version is the failed one")
	}
}

// --- trigger classification -------------------------------------------------

// TestRecoverableDeployFailure: deploy-phase failures are recoverable; build
// failures, usage errors and cancellations are not.
func TestRecoverableDeployFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"build failure", phelixerr.New(phelixerr.CodeBuildFailed, "compile error"), false},
		{"toolchain missing", phelixerr.New(phelixerr.CodeToolchainNotFound, "no go"), false},
		{"usage", phelixerr.New(phelixerr.CodeInvalidArgument, "bad flag"), false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"health check", phelixerr.New(phelixerr.CodeHealthCheckFailed, "unhealthy"), true},
		{"instance start", phelixerr.New(phelixerr.CodeInstanceStartFailed, "spawn"), true},
		{"proxy", phelixerr.New(phelixerr.CodeProxy, "switch failed"), true},
		{"deploy", phelixerr.New(phelixerr.CodeDeployFailed, "rolling failed"), true},
	}
	for _, tc := range cases {
		if got := RecoverableDeployFailure(tc.err); got != tc.want {
			t.Errorf("%s: RecoverableDeployFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- no-op path -------------------------------------------------------------

// TestRunAutoRollback_NoOpWhenNotServing: blue-green aborts happen before the
// traffic switch, so the known-good version never stopped serving — recovery
// must report AlreadyServing without redeploying anything and without writing
// a rollback history record for a rollback that did not happen.
func TestRunAutoRollback_NoOpWhenNotServing(t *testing.T) {
	resetHome(t)
	app := "noop-recovery"
	lgFixture(t, app, map[int]bool{2: true}, 2)
	// v3 "deployed" into the inactive slot but killed by the abort (PID 0).
	if err := Store(&DeployState{
		AppName: app, Mode: ModeBlueGreen, PublicPort: 3000,
		ActiveSlot:    SlotBlue,
		ActiveVersion: 2,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "running", PID: 4242, Port: 4001, Version: 2},
			SlotGreen: {Slot: SlotGreen, Status: "failed", PID: 0, Version: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}

	fl := &httpLauncher{}
	defer fl.close()
	log := &fakeLogger{}

	res := RunAutoRollback(context.Background(), AutoRollbackOptions{
		AppName:       app,
		AppID:         "1",
		FailedVersion: 3,
		Launcher:      fl.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger:        log,
		FailureReason: "Deployment v3 failed: unhealthy",
	})
	if !res.Restored || !res.AlreadyServing {
		t.Fatalf("res = %+v, want Restored=true AlreadyServing=true", res)
	}
	if len(fl.servers) != 0 {
		t.Fatalf("no-op recovery must not start instances, started %d", len(fl.servers))
	}
	// A rollback that did not happen must not appear in history.
	recs, skipped, err := ReadRollbackHistory(app)
	if err != nil || skipped != 0 || len(recs) != 0 {
		t.Fatalf("history must stay empty for a no-op recovery: recs=%d skipped=%d err=%v", len(recs), skipped, err)
	}
}

// --- real recovery ----------------------------------------------------------

// TestRunAutoRollback_RestoresKnownGood_Rolling: a partial rolling rollout
// left the failed version serving a replica; recovery redeploys the
// known-good version through the normal rollback path and records an
// automatic history event.
func TestRunAutoRollback_RestoresKnownGood_Rolling(t *testing.T) {
	resetHome(t)
	app := "rolling-recovery"
	lgFixture(t, app, map[int]bool{2: true}, 2)
	// Partial rollout: replica 0 replaced by v3 (serving), replica 1 still v2.
	if err := Store(&DeployState{
		AppName: app, Mode: ModeRolling, PublicPort: 3000,
		ActiveVersion: 2,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Status: "running", PID: 999999, Port: 40001, Version: 3, BinaryPath: "/nonexistent"},
			"1": {Slot: "1", Status: "stopped", PID: 0, Version: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}

	fl := &httpLauncher{}
	defer fl.close()
	pc := &fakeProxyClient{alive: true}

	res := RunAutoRollback(context.Background(), AutoRollbackOptions{
		AppName:       app,
		AppID:         "1",
		PublicPort:    3000,
		FailedVersion: 3,
		Launcher:      fl.Launch,
		ProxyClient:   pc,
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger:        &fakeLogger{},
		FailureReason: "Deployment v3 failed: replica 0 unhealthy",
	})
	if !res.Restored || res.AlreadyServing {
		t.Fatalf("res = %+v, want real recovery", res)
	}
	if res.FromVer != 3 || res.ToVer != 2 {
		t.Fatalf("from/to = v%d/v%d, want v3/v2", res.FromVer, res.ToVer)
	}
	// Proxy membership was updated during the restore.
	pc.mu.Lock()
	switches := len(pc.switches)
	pc.mu.Unlock()
	if switches == 0 {
		t.Fatal("recovery must update proxy membership")
	}
	// Exactly one automatic history event with the failure reason.
	recs, skipped, err := ReadRollbackHistory(app)
	if err != nil || skipped != 0 {
		t.Fatalf("read history: err=%v skipped=%d", err, skipped)
	}
	if len(recs) != 1 {
		t.Fatalf("history records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Status != RollbackStatusSuccess || rec.Source != RollbackSourceAutomatic {
		t.Fatalf("rec = %+v, want success + automatic", rec)
	}
	if rec.From != "v3" || rec.To != "v2" || rec.Mode != string(ModeRolling) {
		t.Fatalf("rec from/to/mode = %s/%s/%s, want v3/v2/rolling", rec.From, rec.To, rec.Mode)
	}
	if !strings.Contains(rec.Reason, "Deployment v3 failed") {
		t.Fatalf("reason = %q, want the deployment failure reason", rec.Reason)
	}
}

// TestRunAutoRollback_RecoveryFails: when the known-good version also fails
// health, the result must be Restored=false with a CodeAutoRollbackFailed
// error, and history must record the recovery as FAILED — never success.
func TestRunAutoRollback_RecoveryFails(t *testing.T) {
	resetHome(t)
	app := "recovery-fails"
	lgFixture(t, app, map[int]bool{2: true}, 2)
	if err := Store(&DeployState{
		AppName: app, Mode: ModeRolling, PublicPort: 3000,
		ActiveVersion: 2,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Status: "running", PID: 999999, Port: 40001, Version: 3, BinaryPath: "/nonexistent"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// closedLauncher: replacement starts on a port with nothing listening.
	cl := closedLauncher{}
	res := RunAutoRollback(context.Background(), AutoRollbackOptions{
		AppName:       app,
		AppID:         "1",
		PublicPort:    3000,
		FailedVersion: 3,
		Launcher:      cl.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealthShortTimeout()
		},
		Logger:        &fakeLogger{},
		FailureReason: "Deployment v3 failed",
	})
	if res.Restored {
		t.Fatalf("res = %+v, want Restored=false", res)
	}
	if !phelixerr.IsCode(res.Err, phelixerr.CodeAutoRollbackFailed) {
		t.Fatalf("err code = %s, want AUTO_ROLLBACK_FAILED", phelixerr.CodeOf(res.Err))
	}
	recs, _, err := ReadRollbackHistory(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("history records = %d, want 1", len(recs))
	}
	if recs[0].Status != RollbackStatusFailed || recs[0].Source != RollbackSourceAutomatic {
		t.Fatalf("rec = %+v, want failed + automatic", recs[0])
	}
}

// TestRunAutoRollback_NoKnownGood: recovery enabled but nothing to roll back
// to — must fail clearly without fabricating a target.
func TestRunAutoRollback_NoKnownGood(t *testing.T) {
	resetHome(t)
	app := "no-lkg"
	lgFixture(t, app, map[int]bool{}, 0)

	res := RunAutoRollback(context.Background(), AutoRollbackOptions{
		AppName:       app,
		AppID:         "1",
		FailedVersion: 1,
		Logger:        &fakeLogger{},
	})
	if res.Restored {
		t.Fatal("must not fabricate a rollback target")
	}
	if !phelixerr.IsCode(res.Err, phelixerr.CodeRollbackTargetNotFound) {
		t.Fatalf("err code = %s, want ROLLBACK_TARGET_NOT_FOUND", phelixerr.CodeOf(res.Err))
	}
}

// TestAutoRollbackReason_Truncated: derived reasons never exceed the history
// reason bound.
func TestAutoRollbackReason_Truncated(t *testing.T) {
	long := errors.New(strings.Repeat("x", MaxRollbackReasonLength+50))
	got := AutoRollbackReason(13, long)
	if len([]rune(got)) > MaxRollbackReasonLength {
		t.Fatalf("reason length %d exceeds bound", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "Deployment v13 failed: ") {
		t.Fatalf("reason = %q, want deployment failure prefix", got)
	}
}

// Compile-time interface checks for the fake proxy client used above.
var _ ProxyClient = (*fakeProxyClient)(nil)
var _ = proxy.Target{}
