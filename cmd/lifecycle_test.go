package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

// fakeDeployStateBlueGreen builds an in-memory blue-green DeployState with the
// given active slot (no live processes — callers only exercise selection).
func fakeDeployStateBlueGreen(appName, active string) *deploy.DeployState {
	return &deploy.DeployState{
		AppName:    appName,
		Mode:       deploy.ModeBlueGreen,
		PublicPort: 3000,
		ActiveSlot: active,
		Slots: map[string]*deploy.Instance{
			deploy.SlotBlue:  {Slot: deploy.SlotBlue, Status: "running", PID: 0},
			deploy.SlotGreen: {Slot: deploy.SlotGreen, Status: "stopped"},
		},
	}
}

// TestLoadDeployState_ClassicAppsAreNil: apps without a blue-green/rolling
// deploy.json (classic or never-deployed) must not enter the deploy-aware
// lifecycle paths.
func TestLoadDeployState_ClassicAppsAreNil(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	got, err := loadDeployState("never-deployed-app")
	if err != nil {
		t.Fatalf("absent deploy state must be (nil, nil), got error %v", err)
	}
	if got != nil {
		t.Fatalf("absent deploy state must be nil, got %+v", got)
	}
}

// TestLoadDeployState_CorruptIsAnError: a deploy.json that exists but cannot
// be decoded must be surfaced, never collapsed into "classic" — the classic
// path binds the public port the proxy owns.
func TestLoadDeployState_CorruptIsAnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".phelix", "apps", "broken-app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if state, err := loadDeployState("broken-app"); err == nil {
		t.Fatalf("corrupt deploy.json must be an error, got state %+v", state)
	}
}

// TestServingDeployInstance_BlueGreenUsesActiveSlot: the reconciliation
// decision must follow the active slot, not whichever slot happens to run.
func TestServingDeployInstance_BlueGreenUsesActiveSlot(t *testing.T) {
	state := fakeDeployStateBlueGreen("green-active-app", "blue")
	inst := servingDeployInstance(state)
	if inst == nil {
		t.Fatalf("active slot instance must be returned")
	}
	if inst.Slot != "blue" {
		t.Fatalf("slot = %q, want blue", inst.Slot)
	}
}
