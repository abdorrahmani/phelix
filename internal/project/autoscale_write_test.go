package project

import (
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestSetAutoscalingEnabledToggle(t *testing.T) {
	dir := t.TempDir()
	content := `name: api
port: 3000
deploy:
  strategy: rolling
  replicas: 2
  autoscaling:
    enabled: false
    min_replicas: 2
`
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := SetAutoscalingEnabled(dir, true); err != nil {
		t.Fatalf("SetAutoscalingEnabled(true): %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Deploy.Autoscaling.Enabled || cfg.Deploy.Autoscaling.MinReplicas != 2 || cfg.Deploy.Replicas != 2 {
		t.Errorf("after enable: %+v", cfg.Deploy)
	}
	if err := SetAutoscalingEnabled(dir, false); err != nil {
		t.Fatalf("SetAutoscalingEnabled(false): %v", err)
	}
	cfg, err = Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Deploy.Autoscaling.Enabled {
		t.Error("disable did not take effect")
	}
}

func TestSetAutoscalingEnabledCreatesBlock(t *testing.T) {
	dir := t.TempDir()
	content := "name: api\nport: 3000\ndeploy:\n  strategy: rolling\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := SetAutoscalingEnabled(dir, true); err != nil {
		t.Fatalf("SetAutoscalingEnabled: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Deploy.Autoscaling == nil || !cfg.Deploy.Autoscaling.Enabled {
		t.Errorf("autoscaling block not created: %+v", cfg.Deploy)
	}
}

func TestSetAutoscalingEnabledMissingFile(t *testing.T) {
	err := SetAutoscalingEnabled(t.TempDir(), true)
	if err == nil {
		t.Fatal("missing file accepted")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeNotFound {
		t.Errorf("code = %v, want %v", got, phelixerr.CodeNotFound)
	}
}
