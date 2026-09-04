package deploy

import (
	"os"
	"testing"
	"time"
)

// Pruning is deployment-aware: a version whose binary is recorded as running
// (blue-green slot or rolling replica), the deployed version, and the rollback
// target must all survive retention pruning — mid-rollout a fresh build is
// is_current=false, so the old current-only guard let prune delete the binary
// a replica had just started, and rolled-back rollback targets could vanish.
func TestPruneVersions_ProtectsDeployedAndRunningVersions(t *testing.T) {
	resetHome(t)
	const app = "prune-protect"
	const max = 2

	// v1..v4 recorded; v4 is current. max=2 would keep only {v3, v4}.
	vf := &VersionsFile{}
	for _, v := range []int{1, 2, 3, 4} {
		vf.Versions = append(vf.Versions, VersionMeta{
			Version: v, IsCurrent: v == 4, BuiltAt: time.Now(),
		})
		dir, err := versionDir(app, v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveVersions(app, vf); err != nil {
		t.Fatal(err)
	}

	// Deploy state: a running replica on v2 (mid-rollout fresh build is not
	// yet current either) and ActiveVersion v3 — v1 is the rollback target.
	state := &DeployState{
		AppName: app, Mode: ModeRolling, PublicPort: 8000,
		ActiveVersion: 3,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", PID: os.Getpid(), Status: "running", Version: 2},
			"1": {Slot: "1", PID: os.Getpid(), Status: "running", Version: 2},
		},
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}

	log := &fakeLogger{}
	if err := PruneVersions(app, DefaultRetention{Max: max}, log); err != nil {
		t.Fatal(err)
	}

	got, err := LoadVersions(app)
	if err != nil {
		t.Fatal(err)
	}
	alive := map[int]bool{}
	for _, v := range got.Versions {
		alive[v.Version] = true
		if dir, err := versionDir(app, v.Version); err != nil {
			t.Fatal(err)
		} else if _, err := os.Stat(dir); err != nil {
			t.Fatalf("version v%d listed but artifacts gone: %v", v.Version, err)
		}
	}
	for _, must := range []int{2, 3, 4} {
		if !alive[must] {
			t.Fatalf("v%d was pruned; running/deployed versions must survive (alive=%v)", must, alive)
		}
	}
	if alive[1] {
		t.Fatalf("v1 survived; expected it pruned (alive=%v)", alive)
	}
}

// The protection map itself: only instances that are actually alive
// (PID > 0) plus the active and rollback versions are protected.
func TestDeploymentProtectedVersions(t *testing.T) {
	resetHome(t)
	const app = "protect-map"
	state := &DeployState{
		AppName: app, Mode: ModeBlueGreen, PublicPort: 8000,
		ActiveVersion: 5,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, PID: os.Getpid(), Status: "running", Version: 5},
			SlotGreen: {Slot: SlotGreen, Status: "stopped", Version: 4},
		},
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	if err := seedVersionsFile(app, []int{3, 4, 5}); err != nil {
		t.Fatal(err)
	}
	protected := deploymentProtectedVersions(app)
	if !protected[5] {
		t.Fatalf("active version must be protected: %v", protected)
	}
	if !protected[4] {
		t.Fatalf("rollback target (highest version below deployed v5 = v4) must be protected: %v", protected)
	}
	if protected[3] {
		t.Fatalf("v3 is neither running nor the rollback target: %v", protected)
	}
}

func seedVersionsFile(app string, versions []int) error {
	vf := &VersionsFile{}
	for _, v := range versions {
		vf.Versions = append(vf.Versions, VersionMeta{Version: v})
	}
	return saveVersions(app, vf)
}
