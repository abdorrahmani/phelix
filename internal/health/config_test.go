package health

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
) // resetConfigManagerForTest points the package-level singleton at a fresh
// temp HOME so tests neither read nor pollute the developer's real configs.
// Must not run in parallel with other ConfigManager tests.
func resetConfigManagerForTest(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	configMgr = nil
	t.Cleanup(func() { configMgr = nil })
}

func TestEffectiveDeployTier_ExplicitWins(t *testing.T) {
	explicit := &DeployTierConfig{Mode: TierModeHTTP, Path: "/explicit", Retries: 2}
	cfg := &AppHealthConfig{
		Endpoints: map[string]*HealthCheckConfig{
			"default": {Name: "default", URL: "http://localhost:8085/other"},
		},
		DeployTier: explicit,
	}
	got := cfg.EffectiveDeployTier()
	if got != explicit {
		t.Fatalf("explicit DeployTier must win over derived, got %+v", got)
	}
}

func TestEffectiveDeployTier_DerivesFromDefaultEndpoint(t *testing.T) {
	// Regression: an app whose endpoint was persisted (health list/status show
	// it) must be visible to deploys even when the deploy_tier block is
	// missing — e.g. configs written before DeployTier existed, or endpoints
	// added via `health add`.
	cfg := &AppHealthConfig{
		Endpoints: map[string]*HealthCheckConfig{
			"default": {Name: "default", URL: "http://localhost:8085/api/v1/admin/health", Interval: "10s", Retries: 3, Timeout: "10s"},
		},
	}
	got := cfg.EffectiveDeployTier()
	if got == nil {
		t.Fatalf("expected derived tier from default localhost endpoint, got nil")
	}
	if got.Path != "/api/v1/admin/health" {
		t.Fatalf("derived Path = %q, want /api/v1/admin/health", got.Path)
	}
	if got.Mode != TierModeAuto {
		t.Fatalf("derived Mode = %q, want auto", got.Mode)
	}
	if got.Retries != 0 || got.Interval != "" || got.Timeout != "" {
		// Daemon cadence must NOT leak into the deploy probe (it would make
		// the deploy deadline impossible); deploy defaults apply instead.
		t.Fatalf("derived config must carry only Mode/Path, got %+v", got)
	}
}

func TestEffectiveDeployTier_DerivesFromAnyLocalhostEndpoint(t *testing.T) {
	cfg := &AppHealthConfig{
		Endpoints: map[string]*HealthCheckConfig{
			"api": {Name: "api", URL: "http://127.0.0.1:9000/ping"},
		},
	}
	got := cfg.EffectiveDeployTier()
	if got == nil || got.Path != "/ping" {
		t.Fatalf("expected derived Path /ping from 127.0.0.1 endpoint, got %+v", got)
	}
}

func TestEffectiveDeployTier_IgnoresRemoteEndpoints(t *testing.T) {
	// A remote URL says nothing about the newly launched local instance, so it
	// must not be bridged into a deploy-time Tier 1 check.
	cfg := &AppHealthConfig{
		Endpoints: map[string]*HealthCheckConfig{
			"default": {Name: "default", URL: "https://api.example.com/health"},
		},
	}
	if got := cfg.EffectiveDeployTier(); got != nil {
		t.Fatalf("remote endpoint must not become a deploy tier, got %+v", got)
	}
}

func TestEffectiveDeployTier_NilWhenNoEndpoints(t *testing.T) {
	if got := (&AppHealthConfig{}).EffectiveDeployTier(); got != nil {
		t.Fatalf("expected nil for empty config, got %+v", got)
	}
	var nilCfg *AppHealthConfig
	if got := nilCfg.EffectiveDeployTier(); got != nil {
		t.Fatalf("expected nil for nil receiver, got %+v", got)
	}
}

func TestConfigManager_PersistedDeployTierRoundTrip(t *testing.T) {
	// Regression for the deploy health bug: a config saved by `phelix health
	// set` must still be visible to a *fresh process* (deploy). The old bug
	// was not storage — it was the deploy path never initializing the manager
	// — but this pins the full persistence chain end to end: save → reload
	// from disk → GetConfig by app ID → EffectiveDeployTier.
	resetConfigManagerForTest(t)

	cm, err := InitConfigManager()
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	appID := "ab034d93-6540-4870-b8e4-0a10fbc1f755"
	err = cm.SaveConfig(appID, &AppHealthConfig{
		AppID:     appID,
		AppName:   "admin",
		Endpoints: map[string]*HealthCheckConfig{},
		Enabled:   true,
		DeployTier: &DeployTierConfig{
			Mode: TierModeAuto, Path: "/api/v1/admin/health",
			Interval: "10s", Retries: 3, Timeout: "10s",
		},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	// Config is written under the app-name folder, exactly where health
	// list/status (and the deploy provider) read it back.
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".phelix", "apps", "admin", "health.json")); err != nil {
		t.Fatalf("health.json not persisted under app-name folder: %v", err)
	}

	// Simulate a fresh process: wipe the in-memory cache and reload.
	fresh, err := InitConfigManager()
	if err != nil {
		t.Fatalf("re-init: %v", err)
	}
	_ = fresh // InitConfigManager returns the singleton; force a real reload:
	configMgr = nil
	fresh, err = InitConfigManager()
	if err != nil {
		t.Fatalf("fresh init: %v", err)
	}

	got := fresh.GetConfig(appID)
	if got == nil {
		// loadAllConfigs indexes by the app_id stored IN the file; the ID
		// round-trip is exactly what deploy resolution depends on.
		t.Fatalf("config not found by app ID after reload (app_id round-trip broken)")
	}
	tier := got.EffectiveDeployTier()
	if tier == nil || tier.Path != "/api/v1/admin/health" {
		t.Fatalf("reloaded DeployTier = %+v, want Path /api/v1/admin/health", tier)
	}
}

func TestConfigManager_LegacyEndpointOnlyConfigBridges(t *testing.T) {
	// A config file on disk WITHOUT a deploy_tier block (older writer) but
	// with a localhost endpoint must still yield a Tier 1 config after reload.
	resetConfigManagerForTest(t)

	cm, err := InitConfigManager()
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	appID := "legacy-id"
	if err := cm.SaveConfig(appID, &AppHealthConfig{
		AppID:   appID,
		AppName: "legacyapp",
		Endpoints: map[string]*HealthCheckConfig{
			"default": {Name: "default", URL: "http://localhost:8085/healthz"},
		},
		Enabled: true,
		// DeployTier deliberately nil.
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	configMgr = nil
	cm, err = InitConfigManager()
	if err != nil {
		t.Fatalf("fresh init: %v", err)
	}
	tier := cm.GetConfig(appID).EffectiveDeployTier()
	if tier == nil || !strings.HasSuffix(tier.Path, "/healthz") {
		t.Fatalf("legacy endpoint-only config did not bridge to a deploy tier: %+v", tier)
	}
}
