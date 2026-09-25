package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
)

// loadProjectConfig loads phelix.yaml from the current directory. A missing
// file is not an error (nil, nil) — commands keep their existing behavior
// without a config. A present-but-invalid file IS an error: an invalid
// configuration must never be silently ignored.
func loadProjectConfig() (*project.Config, error) {
	dir := currentDirOrError()
	if dir == "" {
		return nil, nil
	}
	return loadProjectConfigFrom(dir)
}

// loadProjectConfigFrom loads phelix.yaml from an explicit directory — the
// same semantics as loadProjectConfig (missing file = nil, nil; invalid file
// = error). Remote matrix commands use it to read the matrix profile of an
// application's project directory, which is not the daemon's working
// directory.
func loadProjectConfigFrom(dir string) (*project.Config, error) {
	cfg, err := project.Load(dir)
	if err != nil {
		if phelixerr.CodeOf(err) == phelixerr.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	return cfg, nil
}

// syncProjectHealth applies the health endpoints declared in phelix.yaml to
// the app's persisted health configuration (the same store that
// `phelix health list/status` and the deploy health tiers read).
//
// phelix.yaml is the desired state: when the health block is present, its
// endpoints replace the app's persisted endpoints on every build/rebuild.
// Apps without a health block keep whatever was configured via the health
// commands. HTTPMetrics opt-ins from `health set` are preserved.
//
// Endpoint identities survive the wholesale replace: ConfigManager.SaveConfig
// carries each endpoint's ID over by name, so a rebuild is an update on the
// backend rather than delete-all-then-recreate. Nothing is pushed over gRPC from
// here — the monitor daemon reports the new configuration in the app's next
// health snapshot (docs/health-backend-contract.md).
func syncProjectHealth(cfg *project.Config, appID, appName string, appPort int) error {
	if cfg == nil || cfg.Health == nil || len(cfg.Health.Endpoints) == 0 {
		return nil
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize health config", err)
	}

	hc := &health.AppHealthConfig{
		AppID:     appID,
		AppName:   appName,
		Endpoints: make(map[string]*health.HealthCheckConfig, len(cfg.Health.Endpoints)),
		Enabled:   true,
	}
	if existing := configMgr.GetConfig(appID); existing != nil {
		hc.HTTPMetrics = existing.HTTPMetrics
	}

	for _, ep := range cfg.Health.Endpoints {
		interval := ep.Interval
		if interval == "" {
			interval = "10s"
		}
		retries := ep.Retries
		if retries == 0 {
			retries = 3
		}
		hc.Endpoints[ep.Name] = &health.HealthCheckConfig{
			Name:          ep.Name,
			URL:           fmt.Sprintf("http://localhost:%d%s", appPort, ep.Path),
			Interval:      interval,
			Retries:       retries,
			ExpectedCodes: "200-299",
			Timeout:       "10s",
		}
	}

	// Deploy tier mirrors `phelix health set`: only Mode and Path, taken from
	// the first declared endpoint. Interval/Retries in the yaml describe the
	// monitoring daemon's cadence and must not inflate the deploy deadline.
	first := cfg.Health.Endpoints[0]
	mode := health.TierModeAuto
	if first.Mode != "" {
		mode = health.DeployTierMode(first.Mode)
	}
	hc.DeployTier = &health.DeployTierConfig{Mode: mode, Path: first.Path}

	if err := configMgr.SaveConfig(appID, hc); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to save health config", err)
	}

	fmt.Printf("  %s Applied %d health endpoint(s) from %s\n",
		color.BlueString("→"), len(cfg.Health.Endpoints), color.CyanString(project.FileName))
	return nil
}

// syncProjectResources persists current runtime policy, not versioned state.
func syncProjectResources(cfg *project.Config, appID string) error {
	if cfg == nil {
		return nil
	}
	if err := cfg.Resources.Validate(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "invalid resource limits", err)
	}
	manager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return phelixerr.New(phelixerr.CodeServer, "invalid app manager type")
	}
	info := manager.Apps[appID]
	if info == nil {
		return phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found in state", appID)
	}
	previous := info.Resources
	if previous == cfg.Resources {
		return nil
	}
	info.Resources = cfg.Resources
	if err := manager.SaveState(); err != nil {
		info.Resources = previous
		return err
	}
	return nil
}

// syncProjectWatching applies the watching value declared in phelix.yaml to
// the app's persisted flag (the same store `phelix watch` writes).
//
// phelix.yaml is the desired state: when the watching key is present, its
// value overrides the app's persisted flag on every build/rebuild — a project
// that declares `watching: enable` stays watched even if the app entry was
// created disabled. A file without the key (older projects) leaves the
// persisted flag untouched, which for a new app means the disabled default.
func syncProjectWatching(cfg *project.Config, appID string) error {
	// No phelix.yaml (loadProjectConfig's nil, nil for a missing file):
	// nothing to apply. Watching must never affect the build itself, so this
	// is a silent no-op, exactly like the health sync's nil guard.
	if cfg == nil {
		return nil
	}
	enabled, ok := cfg.WatchingSetting()
	if !ok {
		return nil
	}

	appManager, isReal := app.Manager.(*app.AppManager)
	if !isReal {
		return phelixerr.New(phelixerr.CodeServer, "invalid app manager type")
	}
	info, exists := appManager.Apps[appID]
	if !exists {
		return phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found in state", appID)
	}

	// Mutate-then-SaveState, the same pattern `phelix watch` and the rebuild
	// flow's NoUpload flag use.
	if info.Watching == enabled {
		return nil
	}
	info.Watching = enabled
	return appManager.SaveState()
}
