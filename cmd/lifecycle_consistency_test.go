package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

// Tests for the reconciled lifecycle record: syncAppInfoWithDeploy must derive
// the app's status from verified process liveness (via deploy.Reconcile),
// never from the status strings persisted in apps.json/deploy.json.

// writeDeployState persists a rolling DeployState under a temp HOME and
// returns the AppInfo wired to it.
func writeDeployState(t *testing.T, app *app.AppInfo, replicas map[string]*deploy.Instance) *deploy.DeployState {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(t.TempDir(), "apps", app.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := &deploy.DeployState{
		AppName:    app.Name,
		Mode:       deploy.ModeRolling,
		PublicPort: 3000,
		Replicas:   replicas,
	}
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestSyncAppInfo_DerivedFromLivenessNotPersistedStatus: deploy.json claims
// every replica "running" but their PIDs are dead -> the app must end up
// stopped, PID 0. This is the exact "list says stopped, replicas say running"
// contradiction, asserted from the record side.
func TestSyncAppInfo_DerivedFromLivenessNotPersistedStatus(t *testing.T) {
	info := &app.AppInfo{ID: "id1", Name: "consistency-app", Status: "running", PID: 424242}
	writeDeployState(t, info, map[string]*deploy.Instance{
		"0": {Slot: "0", Status: "running", PID: 999999, BinaryPath: "/nonexistent"},
		"1": {Slot: "1", Status: "running", PID: 999998, BinaryPath: "/nonexistent"},
	})

	if err := syncAppInfoWithDeploy(info, loadDeployState(info.Name)); err != nil {
		t.Fatal(err)
	}
	if info.Status != "stopped" || info.PID != 0 {
		t.Fatalf("all-dead replicas must reconcile to stopped/0, got %s/%d", info.Status, info.PID)
	}
}

// TestSyncAppInfo_DegradedWhenSomeReplicasDie: one live replica out of two ->
// degraded, auto-start intent kept so the monitor restores the deployment.
func TestSyncAppInfo_DegradedWhenSomeReplicasDie(t *testing.T) {
	info := &app.AppInfo{ID: "id2", Name: "degraded-app", Status: "running"}
	writeDeployState(t, info, map[string]*deploy.Instance{
		"0": {Slot: "0", Status: "running", PID: 999999, BinaryPath: "/nonexistent"},
		"1": {Slot: "1", Status: "running", PID: os.Getpid(), BinaryPath: selfBinaryOrSkip(t)},
	})

	if err := syncAppInfoWithDeploy(info, loadDeployState(info.Name)); err != nil {
		t.Fatal(err)
	}
	if info.Status != "degraded" {
		t.Fatalf("partial liveness must be degraded, got %q", info.Status)
	}
	if !info.AutoStart {
		t.Fatalf("degraded app must keep auto-start intent")
	}
}

// TestSyncAppInfo_StaleRecordNeverOverridesFreshDeploy: apps.json says
// "running" for an app whose replicas all died must not be re-asserted from
// the persisted string — the persisted status must never win over liveness.
func TestSyncAppInfo_StaleRecordNeverOverridesFreshDeploy(t *testing.T) {
	info := &app.AppInfo{ID: "id3", Name: "stale-app", Status: "running", PID: 777}
	writeDeployState(t, info, map[string]*deploy.Instance{
		"0": {Slot: "0", Status: "stopped", PID: 0},
	})

	if err := syncAppInfoWithDeploy(info, loadDeployState(info.Name)); err != nil {
		t.Fatal(err)
	}
	if info.Status != "stopped" || info.PID != 0 {
		t.Fatalf("persisted running status must not survive dead replicas: %s/%d", info.Status, info.PID)
	}
}

func selfBinaryOrSkip(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable to verify against")
	}
	return exe
}
