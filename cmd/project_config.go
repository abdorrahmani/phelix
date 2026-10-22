package cmd

import (
	"fmt"
	"sort"
	"strings"

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
func syncProjectHealth(cfg *project.Config, appID, appName string, appPort int) error {
	if cfg == nil || cfg.Health == nil || len(cfg.Health.Endpoints) == 0 {
		return nil
	}

	configMgr, err := health.InitConfigManager()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to initialize health config", err)
	}

	var dropped []string
	_, err = configMgr.Mutate(appID, appName, func(hc *health.AppHealthConfig) error {
		declared := make(map[string]bool, len(cfg.Health.Endpoints))
		if cfg.Health.Replace {
			for name := range hc.Endpoints {
				declared[name] = false
			}
		}

		if hc.Endpoints == nil {
			hc.Endpoints = make(map[string]*health.HealthCheckConfig, len(cfg.Health.Endpoints))
		}

		for _, ep := range cfg.Health.Endpoints {
			declared[ep.Name] = true
			prev := hc.Endpoints[ep.Name]
			next := &health.HealthCheckConfig{Interval: "10s", Retries: 3, ExpectedCodes: "200-299", Timeout: "10s"}

			if prev != nil {
				*next = *prev
			}

			next.Name = ep.Name
			next.URL = fmt.Sprintf("http://localhost:%d%s", appPort, ep.Path)
			if ep.Interval != "" {
				next.Interval = ep.Interval
			}

			if ep.Retries != 0 {
				next.Retries = ep.Retries
			}

			if ep.ExpectedCodes != "" {
				next.ExpectedCodes = ep.ExpectedCodes
			}
			if ep.Timeout != "" {
				next.Timeout = ep.Timeout
			}
			hc.Endpoints[ep.Name] = next
		}

		if cfg.Health.Replace {
			for name, keep := range declared {
				if !keep {
					dropped = append(dropped, name)
					delete(hc.Endpoints, name)
				}
			}
		}

		first := cfg.Health.Endpoints[0]
		mode := health.TierModeAuto
		if first.Mode != "" {
			mode = health.DeployTierMode(first.Mode)
		}
		hc.DeployTier = &health.DeployTierConfig{Mode: mode, Path: first.Path}
		return nil
	})
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to save health config", err)
	}

	if len(dropped) > 0 {
		sort.Strings(dropped)
		fmt.Printf("  %s health.replace: true removed %d endpoint(s) not declared in %s: %s\n",
			color.YellowString("⚠"), len(dropped), project.FileName, strings.Join(dropped, ", "))
	}

	fmt.Printf("  %s Applied %d health endpoint(s) from %s\n",
		color.BlueString("→"), len(cfg.Health.Endpoints), color.CyanString(project.FileName))
	return nil
}

// syncProjectResources persists current runtime policy, not versioned state.
func syncProjectResources(cfg *project.Config, appID string) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	if err := cfg.Resources.Validate(); err != nil {
		return false, phelixerr.Wrap(phelixerr.CodeConfiguration, "invalid resource limits", err)
	}

	manager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return false, phelixerr.New(phelixerr.CodeServer, "invalid app manager type")
	}

	info := manager.Apps[appID]
	if info == nil {
		return false, phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found in state", appID)
	}

	previous := info.Resources
	if previous == cfg.Resources {
		return false, nil
	}

	info.Resources = cfg.Resources

	return true, nil
}

// syncProjectWatching applies the watching value declared in phelix.yaml to
// the app's persisted flag (the same store `phelix watch` writes).
func syncProjectWatching(cfg *project.Config, appID string) (bool, error) {
	if cfg == nil {
		return false, nil
	}

	enabled, ok := cfg.WatchingSetting()
	if !ok {
		return false, nil
	}

	appManager, isReal := app.Manager.(*app.AppManager)
	if !isReal {
		return false, phelixerr.New(phelixerr.CodeServer, "invalid app manager type")
	}

	info, exists := appManager.Apps[appID]
	if !exists {
		return false, phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found in state", appID)
	}

	if info.Watching == enabled {
		return false, nil
	}
	info.Watching = enabled

	return true, nil
}
