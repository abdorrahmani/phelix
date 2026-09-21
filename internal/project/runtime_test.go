package project

import "testing"

func TestDeployRuntime_DefaultsToNative(t *testing.T) {
	t.Setenv("PHELIX_RUNTIME", "")
	if (&Config{}).DeployRuntime() != RuntimeNative {
		t.Error("no deploy block must resolve to native")
	}
	if (&Config{Deploy: &DeployConfig{}}).DeployRuntime() != RuntimeNative {
		t.Error("empty deploy.runtime must resolve to native")
	}
	var nilCfg *Config
	if nilCfg.DeployRuntime() != RuntimeNative {
		t.Error("nil config must resolve to native")
	}
}

func TestDeployRuntime_ReadsConfig(t *testing.T) {
	t.Setenv("PHELIX_RUNTIME", "")
	cfg := &Config{Deploy: &DeployConfig{Runtime: RuntimeDocker}}
	if cfg.DeployRuntime() != RuntimeDocker {
		t.Errorf("expected docker, got %q", cfg.DeployRuntime())
	}
}

func TestDeployRuntime_EnvOverridesConfig(t *testing.T) {
	t.Setenv("PHELIX_RUNTIME", RuntimeDocker)
	cfg := &Config{Deploy: &DeployConfig{Runtime: RuntimeNative}}
	if cfg.DeployRuntime() != RuntimeDocker {
		t.Error("PHELIX_RUNTIME must override phelix.yaml deploy.runtime")
	}
}

func TestDeployRuntime_InvalidEnvIgnored(t *testing.T) {
	t.Setenv("PHELIX_RUNTIME", "podman")
	cfg := &Config{Deploy: &DeployConfig{Runtime: RuntimeDocker}}
	// An env typo must not silently switch the runtime; it falls through to the
	// validated file value.
	if cfg.DeployRuntime() != RuntimeDocker {
		t.Errorf("invalid env must fall through to config, got %q", cfg.DeployRuntime())
	}
}

func TestValidate_RejectsInvalidRuntime(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Name: "api", Port: 4000, Deploy: &DeployConfig{Runtime: "podman"}}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected an error for invalid deploy.runtime")
	}
}

func TestValidate_RejectsDockerWithClassic(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Name: "api", Port: 4000,
		Deploy: &DeployConfig{Runtime: RuntimeDocker, Strategy: StrategyClassic},
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("docker runtime with classic strategy must be rejected")
	}
}

func TestValidate_AcceptsDockerWithBlueGreen(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Name: "api", Port: 4000,
		Deploy: &DeployConfig{Runtime: RuntimeDocker, Strategy: StrategyBlueGreen},
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("docker + blue-green must be valid: %v", err)
	}
	if got.Deploy.Runtime != RuntimeDocker {
		t.Errorf("runtime round-trip failed: %+v", got.Deploy)
	}
}
