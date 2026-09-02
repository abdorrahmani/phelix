package project

import (
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// HealthEndpoint is one health check endpoint declared in phelix.yaml.
// It mirrors the fields of the persisted health system (internal/health)
// except that endpoints are declared by path; the full URL is derived from
// the application's port when the configuration is applied.
type HealthEndpoint struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	Interval string `yaml:"interval,omitempty"`
	Retries  int    `yaml:"retries,omitempty"`
	Mode     string `yaml:"mode,omitempty"`
}

// HealthConfig groups the health endpoints declared in phelix.yaml.
type HealthConfig struct {
	Endpoints []HealthEndpoint `yaml:"endpoints"`
}

// DeployConfig selects the deployment strategy used by `phelix rebuild`.
type DeployConfig struct {
	Strategy string `yaml:"strategy,omitempty"`
	Replicas int    `yaml:"replicas,omitempty"`
}

// Supported deploy strategies (mapped onto the existing deployment paths).
const (
	StrategyClassic   = "classic"
	StrategyBlueGreen = "blue-green"
	StrategyRolling   = "rolling"
)

// SupportedHealthModes mirrors health.DeployTierMode values.
var supportedHealthModes = map[string]bool{
	"auto": true, "http": true, "tcp-only": true, "none": true,
}

var supportedStrategies = map[string]bool{
	StrategyClassic: true, StrategyBlueGreen: true, StrategyRolling: true,
}

// validate checks the health and deploy sections. Field-level errors name the
// exact yaml path so the user can fix the file without reading source.
func (c *Config) validate() error {
	if c.Deploy != nil {
		s := strings.TrimSpace(c.Deploy.Strategy)
		if s != "" && !supportedStrategies[s] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid deploy.strategy %q\nHint: expected one of: classic, blue-green, rolling", c.Deploy.Strategy)
		}
		if c.Deploy.Replicas < 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: invalid deploy.replicas %d\nHint: replicas must be >= 1", c.Deploy.Replicas)
		}
		if c.Deploy.Replicas > 0 && s != StrategyRolling {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: deploy.replicas requires deploy.strategy: rolling")
		}
	}

	if c.Health == nil {
		return nil
	}
	seen := make(map[string]bool, len(c.Health.Endpoints))
	for i, ep := range c.Health.Endpoints {
		if strings.TrimSpace(ep.Name) == "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] is missing required field: name", i)
		}
		if seen[ep.Name] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: duplicate health endpoint name %q", ep.Name)
		}
		seen[ep.Name] = true
		if !strings.HasPrefix(ep.Path, "/") {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] (%s) has invalid path %q\nHint: path must start with '/' (e.g. /health)", i, ep.Name, ep.Path)
		}
		if ep.Interval != "" {
			if _, err := time.ParseDuration(ep.Interval); err != nil {
				return phelixerr.Newf(phelixerr.CodeConfiguration,
					"configuration error: health.endpoints[%d] (%s) has invalid interval %q\nHint: use a duration like 10s or 1m", i, ep.Name, ep.Interval)
			}
		}
		if ep.Retries < 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] (%s) has invalid retries %d\nHint: retries must be >= 0", i, ep.Name, ep.Retries)
		}
		if ep.Mode != "" && !supportedHealthModes[ep.Mode] {
			return phelixerr.Newf(phelixerr.CodeConfiguration,
				"configuration error: health.endpoints[%d] (%s) has invalid mode %q\nHint: expected one of: auto, http, tcp-only, none", i, ep.Name, ep.Mode)
		}
	}
	return nil
}
