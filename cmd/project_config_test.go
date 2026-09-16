package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/abdorrahmani/phelix/internal/deploy"
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

// A missing phelix.yaml loads as (nil, nil) and syncProjectWatching must be a
// silent no-op on it, not a panic — `phelix build`/`phelix rebuild` in a
// config-less directory call this on every run (regression: the nil deref
// crashed the build after the app entry was already created).
func TestSyncProjectWatchingNilConfig(t *testing.T) {
	if err := syncProjectWatching(nil, "app-1"); err != nil {
		t.Fatalf("syncProjectWatching(nil, ...) = %v, want nil", err)
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
// classic/no-config changes nothing, rolling without replicas defaults to 1,
// and canary/progressive resolve into a rollout plan.
func TestApplyConfigDeployStrategy(t *testing.T) {
	reset := func() {
		rebuildBlueGreen = false
		rebuildReplicas = 0
		rebuildStrategy = ""
		rebuildCanary = 0
		rolloutPlan = nil
	}
	t.Cleanup(reset)
	reset()

	newCmd := func(changed ...string) *cobra.Command {
		c := &cobra.Command{Use: "rebuild"}
		c.Flags().BoolVar(&rebuildBlueGreen, "blue-green", false, "")
		c.Flags().IntVar(&rebuildReplicas, "replicas", 0, "")
		c.Flags().IntVar(&rebuildCanary, "canary", 0, "")
		c.Flags().StringVar(&rebuildStrategy, "strategy", "", "")
		for _, f := range changed {
			_ = c.Flags().Set(f, "1")
		}
		return c
	}

	rollingCfg := &project.Config{Deploy: &project.DeployConfig{Strategy: "rolling", Replicas: 3}}
	canaryPct := project.Percent(5)
	canaryCfg := &project.Config{Deploy: &project.DeployConfig{Strategy: "canary",
		Rollout: &project.RolloutConfig{Canary: &canaryPct, Duration: "1m"}}}
	progressiveCfg := &project.Config{Deploy: &project.DeployConfig{Strategy: "progressive",
		Rollout: &project.RolloutConfig{Steps: []project.RolloutStepConfig{
			{Traffic: 5, Duration: "10s"}, {Traffic: 25}, {Traffic: 100},
		}}}}

	// Deployed state lives under $HOME/.phelix/apps/<name>/deploy.json.
	t.Setenv("HOME", t.TempDir())
	const appName = "web"
	deployedBlueGreen := &deploy.DeployState{AppName: appName, Mode: deploy.ModeBlueGreen, PublicPort: 8080}
	deployedRolling := &deploy.DeployState{
		AppName: appName, Mode: deploy.ModeRolling, PublicPort: 8080,
		Replicas: map[string]*deploy.Instance{
			"0": {Slot: "0"}, "1": {Slot: "1"}, "2": {Slot: "2"}, "3": {Slot: "3"},
		},
	}

	cases := []struct {
		name     string
		cfg      *project.Config
		deployed *deploy.DeployState
		override string
		changed  []string
		wantBG   bool
		wantRep  int
		// wantRollout asserts a rollout plan was resolved with this strategy
		// ("" asserts none was); wantSteps/wantFirstPct refine the check.
		wantRollout  string
		wantSteps    int
		wantFirstPct int
		wantErr      bool
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
		{name: "unknown override is an error", override: "surge", wantErr: true},

		// canary/progressive resolve into a rollout plan.
		{name: "canary from config", cfg: canaryCfg, wantRollout: "canary", wantSteps: 2, wantFirstPct: 5},
		{name: "progressive from config", cfg: progressiveCfg, wantRollout: "progressive", wantSteps: 3, wantFirstPct: 5},
		{name: "override canary uses config rollout", cfg: canaryCfg, override: "canary", wantRollout: "canary", wantSteps: 2, wantFirstPct: 5},
		{name: "override canary without config uses defaults", override: "canary", wantRollout: "canary", wantSteps: 2, wantFirstPct: deploy.DefaultCanaryPercent},
		{name: "override progressive without steps errors", override: "progressive", wantErr: true},
		{name: "explicit --canary builds a plan", cfg: canaryCfg, changed: []string{"canary"}, wantRollout: "canary", wantSteps: 2, wantFirstPct: 1},
		{name: "--canary with --blue-green errors", changed: []string{"canary", "blue-green"}, wantErr: true},
		{name: "--canary with --strategy errors", changed: []string{"canary"}, override: "canary", wantErr: true},

		// deploy.json: the strategy the app is actually running. Inherited only
		// when nothing else names one — a rebuild that names no strategy must
		// not demote a live deployment to classic and delete its state.
		{name: "deployed blue-green is inherited", deployed: deployedBlueGreen, wantBG: true},
		{name: "deployed blue-green is inherited over an empty deploy block", cfg: &project.Config{}, deployed: deployedBlueGreen, wantBG: true},
		{name: "deployed rolling keeps its replica width", deployed: deployedRolling, wantRep: 4},
		{name: "config replicas win over the deployed width", cfg: rollingCfg, deployed: deployedRolling, wantRep: 3},
		{name: "config classic still migrates a deployed rolling app", cfg: &project.Config{Deploy: &project.DeployConfig{Strategy: "classic"}}, deployed: deployedRolling},
		{name: "override classic still migrates a deployed blue-green app", deployed: deployedBlueGreen, override: "classic"},
		{name: "explicit --blue-green wins over deployed rolling", deployed: deployedRolling, changed: []string{"blue-green"}, wantBG: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reset()
			if tc.deployed != nil {
				if err := deploy.Store(tc.deployed); err != nil {
					t.Fatalf("seed deploy state: %v", err)
				}
			} else if err := deploy.RemoveState(appName); err != nil {
				t.Fatalf("clear deploy state: %v", err)
			}
			cmd := newCmd(tc.changed...)
			rebuildStrategy = tc.override
			err := applyConfigDeployStrategy(cmd, tc.cfg, appName)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for an invalid strategy combination")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rebuildBlueGreen != tc.wantBG || rebuildReplicas != tc.wantRep {
				t.Errorf("blue-green=%v replicas=%d, want %v/%d", rebuildBlueGreen, rebuildReplicas, tc.wantBG, tc.wantRep)
			}
			if (rolloutPlan != nil) != (tc.wantRollout != "") {
				t.Fatalf("rolloutPlan = %+v, want strategy %q", rolloutPlan, tc.wantRollout)
			}
			if rolloutPlan != nil {
				if rolloutPlan.Strategy != tc.wantRollout {
					t.Errorf("rollout strategy = %q, want %q", rolloutPlan.Strategy, tc.wantRollout)
				}
				if tc.wantSteps > 0 && len(rolloutPlan.Steps) != tc.wantSteps {
					t.Errorf("rollout steps = %d, want %d", len(rolloutPlan.Steps), tc.wantSteps)
				}
				if tc.wantFirstPct > 0 && rolloutPlan.Steps[0].TrafficPercent != tc.wantFirstPct {
					t.Errorf("first step traffic = %d%%, want %d%%", rolloutPlan.Steps[0].TrafficPercent, tc.wantFirstPct)
				}
				if err := rolloutPlan.Validate(); err != nil {
					t.Errorf("resolved plan is invalid: %v", err)
				}
			}
		})
	}
}
