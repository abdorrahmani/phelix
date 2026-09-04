package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/spf13/cobra"
)

func TestLoadProjectConfigMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := loadProjectConfig()
	if err != nil {
		t.Fatalf("missing config returned error: %v", err)
	}
	if cfg != nil {
		t.Errorf("cfg = %+v, want nil", cfg)
	}
}

func TestLoadProjectConfigInvalid(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte("deploy:\n  strategy: foobar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProjectConfig(); err == nil {
		t.Fatal("invalid config silently ignored, want error")
	} else if phelixerr.CodeOf(err) != phelixerr.CodeConfiguration {
		t.Errorf("code = %q, want CONFIGURATION_ERROR", phelixerr.CodeOf(err))
	}
}

func TestLoadProjectConfigValid(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte("name: api\nport: 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadProjectConfig()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "api" || cfg.Port != 3000 {
		t.Errorf("cfg = %+v", cfg)
	}
}

// TestSyncProjectHealth writes the health endpoints from phelix.yaml into the
// persisted health configuration that `phelix health list/status` and the
// deploy tiers read. HOME is redirected so the test never touches the real
// user phelix state directory.
func TestSyncProjectHealth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	appsDir := filepath.Join(home, ".phelix", "apps")

	cfg := &project.Config{
		Health: &project.HealthConfig{Endpoints: []project.HealthEndpoint{
			{Name: "default", Path: "/health", Interval: "10s", Retries: 3, Mode: "auto"},
			{Name: "readiness", Path: "/ready", Interval: "5s", Retries: 5, Mode: "http"},
		}},
	}

	if err := syncProjectHealth(cfg, "app-1", "myapp", 3000); err != nil {
		t.Fatalf("syncProjectHealth: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(appsDir, "myapp", "health.json"))
	if err != nil {
		t.Fatalf("persisted health config not written: %v", err)
	}
	var got struct {
		AppID     string `json:"app_id"`
		AppName   string `json:"app_name"`
		Endpoints map[string]struct {
			URL      string `json:"url"`
			Interval string `json:"interval"`
			Retries  int    `json:"retries"`
		} `json:"endpoints"`
		DeployTier *struct {
			Mode string `json:"mode"`
			Path string `json:"path"`
		} `json:"deploy_tier"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal health.json: %v", err)
	}
	if got.AppID != "app-1" || got.AppName != "myapp" {
		t.Errorf("app = %s/%s", got.AppID, got.AppName)
	}
	def := got.Endpoints["default"]
	if def.URL != "http://localhost:3000/health" || def.Interval != "10s" || def.Retries != 3 {
		t.Errorf("default endpoint = %+v", def)
	}
	ready := got.Endpoints["readiness"]
	if ready.URL != "http://localhost:3000/ready" || ready.Interval != "5s" || ready.Retries != 5 {
		t.Errorf("readiness endpoint = %+v", ready)
	}
	if got.DeployTier == nil || got.DeployTier.Mode != "auto" || got.DeployTier.Path != "/health" {
		t.Errorf("deploy tier = %+v, want auto//health from first endpoint", got.DeployTier)
	}
}

func TestSyncProjectHealthNoBlockNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	appsDir := filepath.Join(home, ".phelix", "apps")

	if err := syncProjectHealth(&project.Config{Name: "api", Port: 3000}, "app-1", "myapp", 3000); err != nil {
		t.Fatalf("no-op sync returned error: %v", err)
	}
	if _, err := os.Stat(appsDir); !os.IsNotExist(err) {
		t.Errorf("health config written without a health block (err = %v)", err)
	}
}

// TestApplyConfigDeployStrategy verifies the strategy → deploy flags mapping:
// explicit CLI flags win over --strategy, --strategy wins over phelix.yaml,
// classic/no-config changes nothing, rolling without replicas defaults to 1.
func TestApplyConfigDeployStrategy(t *testing.T) {
	reset := func() {
		rebuildBlueGreen = false
		rebuildReplicas = 0
		rebuildStrategy = ""
	}
	t.Cleanup(reset)
	reset()

	newCmd := func(changed ...string) *cobra.Command {
		c := &cobra.Command{Use: "rebuild"}
		c.Flags().BoolVar(&rebuildBlueGreen, "blue-green", false, "")
		c.Flags().IntVar(&rebuildReplicas, "replicas", 0, "")
		c.Flags().StringVar(&rebuildStrategy, "strategy", "", "")
		for _, f := range changed {
			_ = c.Flags().Set(f, "1")
		}
		return c
	}

	rollingCfg := &project.Config{Deploy: &project.DeployConfig{Strategy: "rolling", Replicas: 3}}

	cases := []struct {
		name     string
		cfg      *project.Config
		override string
		changed  []string
		wantBG   bool
		wantRep  int
		wantErr  bool
	}{
		{name: "classic changes nothing", cfg: &project.Config{Deploy: &project.DeployConfig{Strategy: "classic"}}},
		{name: "no deploy block changes nothing", cfg: &project.Config{}},
		{name: "nil config changes nothing"},
		{name: "blue-green", cfg: &project.Config{Deploy: &project.DeployConfig{Strategy: "blue-green"}}, wantBG: true},
		{name: "rolling with replicas", cfg: rollingCfg, wantRep: 3},
		{name: "rolling without replicas defaults 1", cfg: &project.Config{Deploy: &project.DeployConfig{Strategy: "rolling"}}, wantRep: 1},
		{name: "explicit --blue-green wins over config rolling", cfg: rollingCfg, changed: []string{"blue-green"}, wantBG: true},

		// --strategy: one-off override, higher precedence than phelix.yaml.
		{name: "override blue-green beats config classic", cfg: &project.Config{Deploy: &project.DeployConfig{Strategy: "classic"}}, override: "blue-green", wantBG: true},
		{name: "override classic beats config rolling", cfg: rollingCfg, override: "classic"},
		{name: "override rolling takes replicas from config", cfg: rollingCfg, override: "rolling", wantRep: 3},
		{name: "override rolling without config defaults 1", override: "rolling", wantRep: 1},
		{name: "explicit --replicas wins over override blue-green", cfg: rollingCfg, override: "blue-green", changed: []string{"replicas"}, wantRep: 1},
		{name: "unknown override is an error", override: "canary", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset()
			cmd := newCmd(tc.changed...)
			rebuildStrategy = tc.override
			err := applyConfigDeployStrategy(cmd, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for an unsupported strategy")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rebuildBlueGreen != tc.wantBG || rebuildReplicas != tc.wantRep {
				t.Errorf("blue-green=%v replicas=%d, want %v/%d", rebuildBlueGreen, rebuildReplicas, tc.wantBG, tc.wantRep)
			}
		})
	}
}
