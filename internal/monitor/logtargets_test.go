package monitor

import (
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

// A worker must expose one log target per replica, named after the replica,
// instead of the single combined app file a service uses.
func TestAppLogTargets_ExpandsWorkerReplicas(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PHELIX_DATA_DIR", "")

	state := &deploy.DeployState{
		AppName: "wk",
		Mode:    deploy.ModeWorker,
		Replicas: map[string]*deploy.Instance{
			"0": {Slot: "0", BinaryPath: "/tmp/binary", Version: 2},
			"1": {Slot: "1", BinaryPath: "/tmp/binary", Version: 2},
		},
	}
	if err := deploy.Store(state); err != nil {
		t.Fatalf("store deploy state: %v", err)
	}

	targets := AppLogTargets(app.AppListItem{ID: "app-w", Name: "wk"})
	if len(targets) != 2 {
		t.Fatalf("targets = %+v, want two replicas", targets)
	}
	for i, target := range targets {
		wantReplica := string(rune('0' + i))
		if target.Replica != wantReplica {
			t.Fatalf("target %d replica = %q, want %q", i, target.Replica, wantReplica)
		}
		if target.Version != "v2" {
			t.Fatalf("target %d version = %q, want v2", i, target.Version)
		}
		if filepath.Dir(target.Path) != filepath.Join(home, ".phelix", "logs") {
			t.Fatalf("target %d path = %q", i, target.Path)
		}
	}
}

func TestAppLogTargetsFindsServiceInstanceOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := &deploy.DeployState{AppName: "service-output", Mode: deploy.ModeRolling,
		Replicas: map[string]*deploy.Instance{"0": {Slot: "0", BinaryPath: "/app", Port: 9123, Version: 4}}}
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	targets := AppLogTargets(app.AppListItem{ID: "service", Name: state.AppName})
	if len(targets) != 1 || targets[0].Path != deploy.InstanceLogFilePath(home, "/app", 9123) || targets[0].Version != "v4" {
		t.Fatal("service output does not match launch destination", targets)
	}
}
