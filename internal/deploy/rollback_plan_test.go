package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePlanFixture seeds ~/.phelix/apps/<name>/ with versions.json, builds/vN
// binaries, env snapshots and an optional deploy state — everything
// PlanRollback reads. Returns the fake home dir.
func writePlanFixture(t *testing.T, name string, versions []VersionMeta, state *DeployState) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	appDir := filepath.Join(home, ".phelix", "apps", name)

	vf := &VersionsFile{Versions: versions}
	data, err := json.MarshalIndent(vf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "versions.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		vdir := filepath.Join(appDir, "builds", fmt.Sprintf("v%d", v.Version))
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vdir, "binary"), []byte("bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if state != nil {
		sd, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(appDir, "deploy.json"), sd, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func planVersions() []VersionMeta {
	return []VersionMeta{
		{Version: 1, GitCommit: "aaaa1111", BuiltAt: time.Now().Add(-48 * time.Hour), SizeBytes: 1024},
		{Version: 2, Tag: "stable", GitCommit: "bbbb2222", BuiltAt: time.Now().Add(-24 * time.Hour), SizeBytes: 2048},
		{Version: 3, GitCommit: "cccc3333", BuiltAt: time.Now(), SizeBytes: 4096, IsCurrent: true},
	}
}

func TestPlanRollbackBlueGreen(t *testing.T) {
	name := "bgapp"
	home := writePlanFixture(t, name, planVersions(), &DeployState{
		AppName:       name,
		Mode:          ModeBlueGreen,
		PublicPort:    3000,
		ActiveSlot:    SlotGreen,
		ActiveVersion: 3,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: {Slot: SlotGreen, Status: "running", PID: 4242, Port: 31001, Version: 3},
		},
	})

	envDir := filepath.Join(home, ".phelix", "apps", name, "env")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "v2.enc"), []byte("enc"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanRollback(name, RollbackPlanInput{Target: 2, ClassicPort: 8080, ClassicDestBin: "/tmp/x"})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}

	if plan.Strategy != "blue-green" {
		t.Errorf("Strategy = %q, want blue-green", plan.Strategy)
	}
	if plan.CurrentVersion != 3 || plan.TargetVersion != 2 {
		t.Errorf("versions = v%d → v%d, want v3 → v2", plan.CurrentVersion, plan.TargetVersion)
	}
	if plan.TargetTag != "stable" {
		t.Errorf("TargetTag = %q, want stable", plan.TargetTag)
	}
	if plan.CurrentSlot != "green" || plan.TargetSlot != "blue" {
		t.Errorf("slots = %s → %s, want green → blue", plan.CurrentSlot, plan.TargetSlot)
	}
	if plan.PublicPort != 3000 {
		t.Errorf("PublicPort = %d, want 3000", plan.PublicPort)
	}
	if !plan.EnvSnapshotAvailable {
		t.Error("EnvSnapshotAvailable = false, want true (v2.enc exists)")
	}
	if plan.HealthCheck == "" {
		t.Error("HealthCheck empty")
	}
	if len(plan.Steps) == 0 {
		t.Fatal("no steps")
	}
	// Steps must mention the traffic switch and the drain.
	foundSwitch, foundDrain := false, false
	for _, s := range plan.Steps {
		if contains(s, "Switch proxy traffic") {
			foundSwitch = true
		}
		if contains(s, "Drain") {
			foundDrain = true
		}
	}
	if !foundSwitch || !foundDrain {
		t.Errorf("steps missing switch/drain; steps=%v", plan.Steps)
	}
	_ = home
}

func TestPlanRollbackRolling(t *testing.T) {
	name := "rollapp"
	writePlanFixture(t, name, planVersions(), &DeployState{
		AppName:       name,
		Mode:          ModeRolling,
		PublicPort:    3000,
		ActiveVersion: 3,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Status: "running", PID: 111, Port: 31001, Version: 3},
			"1": {Slot: "1", Status: "running", PID: 222, Port: 31002, Version: 3},
		},
	})

	plan, err := PlanRollback(name, RollbackPlanInput{Target: 1})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if plan.Strategy != "rolling" {
		t.Errorf("Strategy = %q, want rolling", plan.Strategy)
	}
	if plan.Replicas != 2 {
		t.Errorf("Replicas = %d, want 2", plan.Replicas)
	}
	// One replace step per replica + proxy ensure + artifact + promote.
	if len(plan.Steps) < 4 {
		t.Errorf("Steps = %v, want >= 4", plan.Steps)
	}
	if !contains(plan.Steps[3], "replica 0") {
		t.Errorf("steps[3] = %q, want replica 0 replacement", plan.Steps[3])
	}
}

func TestPlanRollbackClassic(t *testing.T) {
	name := "classicapp"
	writePlanFixture(t, name, planVersions(), nil)

	plan, err := PlanRollback(name, RollbackPlanInput{
		Target:         2,
		ClassicRunning: true,
		ClassicPort:    8080,
		ClassicDestBin: "/home/u/.phelix/apps/classicapp/app_x",
	})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if plan.Strategy != "classic" {
		t.Errorf("Strategy = %q, want classic", plan.Strategy)
	}
	if !plan.Downtime {
		t.Error("Downtime = false, want true (instance running)")
	}
	if plan.HealthCheck == "" || !contains(plan.HealthCheck, "none") {
		t.Errorf("HealthCheck = %q, want none (classic)", plan.HealthCheck)
	}
	if !contains(plan.Steps[0], "Stop") {
		t.Errorf("steps[0] = %q, want stop step", plan.Steps[0])
	}
}

func TestPlanRollbackMissingBinaryBlocked(t *testing.T) {
	name := "missingbin"
	home := writePlanFixture(t, name, planVersions(), nil)
	// Delete the target's binary.
	bin := filepath.Join(home, ".phelix", "apps", name, "builds", "v2", "binary")
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}

	_, err := PlanRollback(name, RollbackPlanInput{Target: 2, ClassicPort: 8080, ClassicDestBin: "/tmp/x"})
	if err == nil {
		t.Fatal("PlanRollback succeeded, want error for missing binary")
	}
}

func TestPlanRollbackMissingEnvSnapshotWarns(t *testing.T) {
	name := "missenv"
	writePlanFixture(t, name, planVersions(), &DeployState{
		AppName: name, Mode: ModeBlueGreen, PublicPort: 3000, ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{SlotGreen: {Slot: SlotGreen, Status: "running", PID: 1, Port: 2}},
	})

	plan, err := PlanRollback(name, RollbackPlanInput{Target: 2})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if plan.EnvSnapshotAvailable {
		t.Error("EnvSnapshotAvailable = true, want false")
	}
	found := false
	for _, w := range plan.Warnings {
		if contains(w, "snapshot is missing") {
			found = true
		}
	}
	if !found {
		t.Errorf("no missing-snapshot warning in %v", plan.Warnings)
	}
}

func TestPlanRollbackSameVersionRejected(t *testing.T) {
	name := "samever"
	writePlanFixture(t, name, planVersions(), nil)

	_, err := PlanRollback(name, RollbackPlanInput{Target: 3})
	if err == nil {
		t.Fatal("PlanRollback succeeded for current version, want error")
	}
}

func TestPlanRollbackUnknownVersionRejected(t *testing.T) {
	name := "unkver"
	writePlanFixture(t, name, planVersions(), nil)

	_, err := PlanRollback(name, RollbackPlanInput{Target: 9})
	if err == nil {
		t.Fatal("PlanRollback succeeded for unknown version, want error")
	}
}

func TestPlanRollbackFarDistanceWarns(t *testing.T) {
	name := "farapp"
	vers := []VersionMeta{
		{Version: 1, BuiltAt: time.Now(), SizeBytes: 1},
		{Version: 12, BuiltAt: time.Now(), SizeBytes: 1, IsCurrent: true},
	}
	writePlanFixture(t, name, vers, nil)

	plan, err := PlanRollback(name, RollbackPlanInput{Target: 1})
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	found := false
	for _, w := range plan.Warnings {
		if contains(w, "11 versions behind") {
			found = true
		}
	}
	if !found {
		t.Errorf("no distance warning in %v", plan.Warnings)
	}
}

// TestPlanRollbackNoMutation snapshots every state file the rollback path can
// touch, plans twice, and asserts byte-identical state — the core dry-run
// guarantee at the plan level.
func TestPlanRollbackNoMutation(t *testing.T) {
	name := "nomut"
	home := writePlanFixture(t, name, planVersions(), &DeployState{
		AppName:       name,
		Mode:          ModeBlueGreen,
		PublicPort:    3000,
		ActiveSlot:    SlotGreen,
		ActiveVersion: 3,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: {Slot: SlotGreen, Status: "running", PID: 4242, Port: 31001, Version: 3},
		},
	})
	appDir := filepath.Join(home, ".phelix", "apps", name)
	// Target env snapshot present, plus a pre-existing rollback.log to prove
	// the planner never appends to it.
	envDir := filepath.Join(appDir, "env")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "v2.enc"), []byte("enc"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(appDir, "rollback.log")
	if err := os.WriteFile(logPath, []byte("preexisting\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot := func() map[string]string {
		out := map[string]string{}
		paths := []string{
			filepath.Join(appDir, "versions.json"),
			filepath.Join(appDir, "deploy.json"),
			logPath,
			filepath.Join(appDir, "deploy.lock"),
			filepath.Join(appDir, "current"),
			filepath.Join(appDir, "health.json"),
		}
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				out[p] = "<absent>"
				// current is a symlink; Lstat would confirm, absence in the map
				// is what matters.
				continue
			}
			out[p] = string(data)
		}
		return out
	}

	before := snapshot()
	for i := 0; i < 3; i++ {
		if _, err := PlanRollback(name, RollbackPlanInput{Target: 2}); err != nil {
			t.Fatalf("run %d: PlanRollback: %v", i, err)
		}
	}
	after := snapshot()
	for p, v := range before {
		if after[p] != v {
			t.Errorf("state changed after dry-run planning: %s", p)
		}
	}
	// Extra: the lock file must not have been created by planning.
	if _, err := os.Stat(filepath.Join(appDir, "deploy.lock")); err == nil {
		t.Error("deploy.lock exists after planning; PlanRollback must not create it")
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
