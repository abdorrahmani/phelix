package deploy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
)

// Regression tests for the state-consistency audit. The invariant under test:
// after a successful rollback, deploy.json describes the deployment that is
// actually serving (fresh PIDs/ports/version), never the pre-rollback
// snapshot. The pre-fix ExecuteRollback re-stored its stale in-memory state
// after the strategy deploy had persisted the new reality, resurrecting dead
// PIDs as "running" and reverting the active slot/version — which made
// `phelix list` report stopped while the new instances served traffic, and
// made the recorded active slot disagree with the proxy primary.

// rollbackFixture prepares versions v1..v2 (v2 current) plus a pre-existing
// rolling deployment over n replicas whose recorded instances use deadPID
// (definitely not running) and stalePort. Binaries are copied from the test
// binary so identity verification behaves as in production.
func rollbackFixture(t *testing.T, app string, n int, deadPID int, stalePort int) {
	t.Helper()
	for v := 1; v <= 2; v++ {
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
	for v := 1; v <= 2; v++ {
		vf.Versions = append(vf.Versions, VersionMeta{Version: v, BuiltAt: time.Now(), IsCurrent: v == 2})
	}
	if err := saveVersions(app, vf); err != nil {
		t.Fatal(err)
	}
	replicas := make(map[string]*Instance, n)
	for i := 0; i < n; i++ {
		key := strconv.Itoa(i)
		replicas[key] = &Instance{
			Slot: key, PID: deadPID, Port: stalePort,
			BinaryPath: "/nonexistent/stale-binary", Status: "running", Version: 2,
		}
	}
	if err := Store(&DeployState{
		AppName: app, Mode: ModeRolling, PublicPort: 3000,
		Replicas: replicas, ActiveVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestExecuteRollback_Rolling_PersistsPostRollbackState is the regression test
// for the stale-state clobber: every replica record must describe the
// replacement the rollback just launched (alive PID, fresh port, target
// version), and ActiveVersion/LastRollback must match the target.
func TestExecuteRollback_Rolling_PersistsPostRollbackState(t *testing.T) {
	resetHome(t)
	app := "rollback-rolling"
	rollbackFixture(t, app, 2, 999999, 40000)

	fl := &httpLauncher{}
	defer fl.close()
	log := &fakeLogger{}

	err := ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      2,
		Launcher:      fl.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealth()
		},
		Logger: log,
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}

	after, err := Load(app)
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveVersion != 1 {
		t.Fatalf("ActiveVersion = %d, want 1 (must describe the version now serving)", after.ActiveVersion)
	}
	if after.LastRollback == nil || after.LastRollback.ToVersion != 1 || after.LastRollback.FromVersion != 2 {
		t.Fatalf("LastRollback = %+v, want 2 -> 1", after.LastRollback)
	}
	if len(fl.servers) == 0 {
		t.Fatalf("rollback must launch replacement instances")
	}
	launchedPorts := map[int]bool{}
	for _, srv := range fl.servers {
		launchedPorts[srv.Listener.Addr().(*net.TCPAddr).Port] = true
	}
	for key, inst := range after.Replicas {
		if inst.PID == 999999 {
			t.Fatalf("replica %s still carries the stale pre-rollback PID — state was clobbered", key)
		}
		if inst.PID <= 0 {
			t.Fatalf("replica %s has no PID after successful rollback", key)
		}
		if inst.Version != 1 {
			t.Fatalf("replica %s Version = %d, want 1", key, inst.Version)
		}
		if inst.Port == 40000 {
			t.Fatalf("replica %s kept the stale port — state was clobbered", key)
		}
		if !launchedPorts[inst.Port] {
			t.Fatalf("replica %s port %d was not launched by this rollback", key, inst.Port)
		}
	}
}

// --- invariants -------------------------------------------------------------

// TestInvariant_ServingInstanceMustBeIdentityVerified: a rolling state whose
// first replica is dead but a later one is alive must report running — the app
// is still serving — and must name the live replica, never the dead one.
func TestInvariant_ServingInstanceMustBeIdentityVerified(t *testing.T) {
	resetHome(t)
	exe := selfBinary(t)
	state := &DeployState{
		AppName: "inv-serving",
		Mode:    ModeRolling,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Status: "running", PID: 999999, BinaryPath: "/nonexistent", Port: 1},
			"1": {Slot: "1", Status: "running", PID: os.Getpid(), BinaryPath: exe, Port: 2},
			"2": {Slot: "2", Status: "running", PID: 999998, BinaryPath: "/nonexistent", Port: 3},
		},
	}
	d := DecideLifecycle(state)
	if d.Status != "degraded" {
		t.Fatalf("1 of 3 replicas alive must be degraded, got %q", d.Status)
	}
	if d.PID != os.Getpid() {
		t.Fatalf("serving PID = %d, want the live replica %d", d.PID, os.Getpid())
	}
	if d.Alive != 1 || d.Desired != 3 {
		t.Fatalf("alive/desired = %d/%d, want 1/3", d.Alive, d.Desired)
	}
	if si := state.ServingInstance(); si == nil || si.PID != os.Getpid() {
		t.Fatalf("ServingInstance must be the alive replica, got %+v", si)
	}
}

// TestInvariant_AllDeadMeansStopped: every claimed instance dead => stopped,
// never running — even though the records still claim "running".
func TestInvariant_AllDeadMeansStopped(t *testing.T) {
	resetHome(t)
	state := &DeployState{
		AppName: "inv-alldead",
		Mode:    ModeRolling,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Status: "running", PID: 999999, BinaryPath: "/nonexistent"},
			"1": {Slot: "1", Status: "running", PID: 999998, BinaryPath: "/nonexistent"},
		},
	}
	if d := DecideLifecycle(state); d.Status != "stopped" {
		t.Fatalf("all-dead must be stopped, got %q", d.Status)
	}
}

// TestInvariant_ReconcileRepairsPersistedState: Reconcile must clear records
// whose process is dead/recycled, correct lying status strings, persist the
// repair, and never touch live records.
func TestInvariant_ReconcileRepairsPersistedState(t *testing.T) {
	resetHome(t)
	exe := selfBinary(t)
	app := "inv-reconcile"
	state := &DeployState{
		AppName: app,
		Mode:    ModeRolling,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Status: "running", PID: 999999, BinaryPath: "/nonexistent", Port: 1, Version: 7},
			"1": {Slot: "1", Status: "stopped", PID: os.Getpid(), BinaryPath: exe, Port: 2, Version: 7},
		},
		ActiveVersion: 7,
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if state.Replicas["0"].PID != 0 || state.Replicas["0"].Status != "stopped" {
		t.Fatalf("dead replica not reaped: %+v", state.Replicas["0"])
	}
	if state.Replicas["1"].Status != "running" {
		t.Fatalf("live replica status not corrected: %+v", state.Replicas["1"])
	}
	reloaded, err := Load(app)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Replicas["0"].PID != 0 {
		t.Fatalf("repair not persisted: replica 0 PID = %d", reloaded.Replicas["0"].PID)
	}
	// Version/binary metadata must survive the reap for later start/rollback.
	if reloaded.Replicas["0"].Version != 7 {
		t.Fatalf("reap must keep version metadata, got %+v", reloaded.Replicas["0"])
	}
}

// TestReapStaleInstances_ClearsOnlyDeadPIDs: reap must clear definitely-dead
// PIDs but leave live-but-unverifiable records alone (they may still serve).
func TestReapStaleInstances_ClearsOnlyDeadPIDs(t *testing.T) {
	state := &DeployState{
		AppName: "inv-reap",
		Mode:    ModeRolling,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Status: "running", PID: 999999, BinaryPath: "/nonexistent"},
			"1": {Slot: "1", Status: "running", PID: os.Getpid(), BinaryPath: "/nonexistent"},
		},
	}
	ReapStaleInstances(state)
	if state.Replicas["0"].PID != 0 {
		t.Fatalf("dead PID not reaped: %+v", state.Replicas["0"])
	}
	if state.Replicas["1"].PID != os.Getpid() {
		t.Fatalf("live PID must survive reap even when unverifiable: %+v", state.Replicas["1"])
	}
}

// TestExecuteRollback_FailedRollbackLeavesPostDeployStateIntact: a rollback
// that fails before switching must not rewrite deploy.json at all — the
// previously active deployment stays exactly as persisted.
func TestExecuteRollback_FailedRollbackLeavesPostDeployStateIntact(t *testing.T) {
	resetHome(t)
	app := "rollback-fail-intact"
	rollbackFixture(t, app, 2, 999999, 40000)

	_, err := Load(app)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := Load(app)

	err = ExecuteRollback(context.Background(), RollbackOptions{
		AppName:       app,
		TargetVersion: 1,
		Replicas:      2,
		Launcher:      closedLauncher{}.Launch,
		ProxyClient:   &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealthShortTimeout()
		},
		Logger: &fakeLogger{},
	})
	if err == nil {
		t.Fatal("expected rollback to fail with unhealthy target")
	}
	after, _ := Load(app)
	if after.ActiveVersion != before.ActiveVersion {
		t.Fatalf("failed rollback changed ActiveVersion: %d -> %d", before.ActiveVersion, after.ActiveVersion)
	}
	for key, inst := range before.Replicas {
		afterInst := after.Replicas[key]
		if inst.PID > 0 && pidAlive(inst.PID) {
			// A live replica is the serving deployment: it must be untouched.
			if afterInst.PID != inst.PID || afterInst.Port != inst.Port {
				t.Fatalf("failed rollback mutated live replica %s: %+v -> %+v", key, inst, afterInst)
			}
			continue
		}
		// A dead record may only be repaired (cleared), never repurposed.
		if afterInst.PID != 0 && afterInst.PID != inst.PID {
			t.Fatalf("failed rollback repurposed stale replica %s: %+v -> %+v", key, inst, afterInst)
		}
		if afterInst.Version != inst.Version {
			t.Fatalf("failed rollback changed replica %s version: %d -> %d", key, inst.Version, afterInst.Version)
		}
	}
	if after.LastRollback != nil {
		t.Fatalf("failed rollback must not record a successful rollback: %+v", after.LastRollback)
	}
}
