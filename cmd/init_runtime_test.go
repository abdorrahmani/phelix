package cmd

import (
	"os"
	"testing"

	"github.com/abdorrahmani/phelix/internal/project"
)

// TestInitDefaultsToNativeRuntime: with no --runtime and non-interactive
// (--yes), init writes the native runtime and keeps the classic strategy.
func TestInitDefaultsToNativeRuntime(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/api\n\ngo 1.21\n",
		"main.go": goPORTMain,
	})
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	initName, initPort, initRuntime, initYes = "api", 4000, "", true
	defer func() { initRuntime = "" }()
	if err := InitCmd.RunE(InitCmd, nil); err != nil {
		t.Fatalf("init RunE: %v", err)
	}

	cfg, err := project.Load(dir)
	if err != nil {
		t.Fatalf("created config does not load: %v", err)
	}
	if cfg.Deploy == nil || cfg.Deploy.Runtime != project.RuntimeNative {
		t.Errorf("runtime = %+v, want native", cfg.Deploy)
	}
	if cfg.Deploy.Strategy != project.StrategyClassic {
		t.Errorf("native default strategy = %q, want classic", cfg.Deploy.Strategy)
	}
}

// TestInitDockerRuntimeDefaultsBlueGreen: --runtime docker writes docker and
// upgrades the strategy to blue-green (docker + classic is rejected by
// validation, so init must produce a valid file).
func TestInitDockerRuntimeDefaultsBlueGreen(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/api\n\ngo 1.21\n",
		"main.go": goPORTMain,
	})
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	initName, initPort, initRuntime, initYes = "api", 4000, project.RuntimeDocker, true
	defer func() { initRuntime = "" }()
	if err := InitCmd.RunE(InitCmd, nil); err != nil {
		t.Fatalf("init RunE: %v", err)
	}

	cfg, err := project.Load(dir)
	if err != nil {
		t.Fatalf("docker config must be valid and load: %v", err)
	}
	if cfg.Deploy.Runtime != project.RuntimeDocker {
		t.Errorf("runtime = %q, want docker", cfg.Deploy.Runtime)
	}
	if cfg.Deploy.Strategy != project.StrategyBlueGreen {
		t.Errorf("docker strategy = %q, want blue-green", cfg.Deploy.Strategy)
	}
}

// TestInitRejectsInvalidRuntime: a --runtime typo fails before writing.
func TestInitRejectsInvalidRuntime(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"go.mod":  "module example.com/api\n\ngo 1.21\n",
		"main.go": goPORTMain,
	})
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)

	initName, initPort, initRuntime, initYes = "api", 4000, "podman", true
	defer func() { initRuntime = "" }()
	if err := InitCmd.RunE(InitCmd, nil); err == nil {
		t.Error("invalid --runtime podman must be rejected")
	}
	// No file should have been written.
	if project.Exists(dir) {
		t.Error("init wrote a config despite an invalid runtime")
	}
}
