package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/abdorrahmani/phelix/internal/autoscale"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/hooks"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

func TestMonitorRuntimeUsesConfiguredDataDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", root)
	name := "isolated-worker"
	state := &deploy.DeployState{AppName: name, AppID: "isolated-id", Mode: deploy.ModeWorker}
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	if got := deploy.WorkerAppNames(); !reflect.DeepEqual(got, []string{name}) {
		t.Fatalf("worker discovery = %v, want only %s", got, name)
	}
	cm, err := health.InitConfigManager()
	if err != nil {
		t.Fatal(err)
	}
	if err := cm.SaveConfig(state.AppID, &health.AppHealthConfig{AppName: name}); err != nil {
		t.Fatal(err)
	}
	if err := autoscale.SavePauseState(name, autoscale.PauseState{Paused: true}); err != nil {
		t.Fatal(err)
	}
	if err := autoscale.AppendHistoryRecord(name, autoscale.HistoryRecord{Kind: autoscale.HistoryDecision}); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"deploy.json", "health.json", "autoscale.json", "autoscale_history.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, "apps", name, file)); err != nil {
			t.Errorf("%s was not written inside the configured directory: %v", file, err)
		}
	}
	paths := []struct {
		get  func() (string, error)
		want string
	}{
		{func() (string, error) { return deploy.AppDataDir(name) }, filepath.Join(root, "apps", name)},
		{hooks.NewStore(name).Dir, filepath.Join(root, "apps", name, "hooks")},
		{proxy.DefaultSocketPath, filepath.Join(root, "proxy.sock")},
		{proxy.DefaultStatePath, filepath.Join(root, "proxy-state.json")},
	}
	for _, path := range paths {
		got, err := path.get()
		if err != nil || got != path.want {
			t.Errorf("runtime path = %q, error: %v, want %q", got, err, path.want)
		}
	}
	if got, want := deploy.InstanceLogFilePath("/unused", "binary", 8080), filepath.Join(root, "logs", "deploy_binary_8080.log"); got != want {
		t.Errorf("service output path = %q, want %q", got, want)
	}
	if got, want := deploy.WorkerLogFilePath("/unused", "binary", "0"), filepath.Join(root, "logs", "deploy_binary_replica-0.log"); got != want {
		t.Errorf("worker output path = %q, want %q", got, want)
	}
	if err := autoscale.SavePauseState("../outside", autoscale.PauseState{}); err == nil {
		t.Error("autoscale accepted a name outside the application directory")
	}
}
