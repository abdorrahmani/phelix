package monitor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/processex"
)

// A worker must reach the dashboard as a worker with live replica state, not as
// a service carrying whatever AppInfo last recorded.
func TestCollectAppDetails_ReportsWorkerTypeAndLiveStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PHELIX_DATA_DIR", "")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte("name: wk\ntype: worker\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	id, err := processex.Identity(os.Getpid())
	if err != nil {
		t.Fatalf("process identity: %v", err)
	}

	state := &deploy.DeployState{
		AppName: "wk",
		Mode:    deploy.ModeWorker,
		Replicas: map[string]*deploy.Instance{
			"0": {Slot: "0", Status: "running", PID: os.Getpid(), StartTicks: id.StartTime, BinaryPath: exe},
			"1": {Slot: "1", Status: "stopped", PID: 0},
		},
	}
	if err := deploy.Store(state); err != nil {
		t.Fatalf("store deploy state: %v", err)
	}

	orig := app.Manager
	app.Manager = &watchingAppStub{items: []app.AppListItem{
		{ID: "app-w", Name: "wk", Directory: dir, Status: "stopped", Watching: true},
	}}
	t.Cleanup(func() { app.Manager = orig })

	details := NewMetricsCollector().CollectAppDetails()
	if len(details) != 1 {
		t.Fatalf("details = %+v, want one app", details)
	}
	got := details[0]
	if got.WorkloadType != "worker" {
		t.Fatalf("WorkloadType = %q, want worker", got.WorkloadType)
	}
	if got.Status != "degraded" {
		t.Fatalf("Status = %q, want degraded", got.Status)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("PID = %d, want live replica %d", got.PID, os.Getpid())
	}
	if got.Port != 0 {
		t.Fatalf("Port = %d, want 0", got.Port)
	}
}
