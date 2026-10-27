// Package workspace implements multi-service workspaces. A single
// phelix.workspace.yaml manifest, or a native go.work / Cargo workspace when
// no manifest is present, discovers the member services of a monorepo and
// lets Phelix build, deploy, and observe them as one unit instead of one
// Phelix invocation per service.
//
// The package owns three concerns: manifest parsing and validation, native
// workspace discovery, and the dependency graph that orders orchestration.
// Execution stays in cmd, which reuses the existing single-app build and
// deploy paths per service.
package workspace

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"go.yaml.in/yaml/v3"
)

// FileName is the workspace-level Phelix manifest, read from the workspace
// root directory.
const FileName = "phelix.workspace.yaml"

// Strategy values accepted for a manifest service. Canary and progressive
// rollouts are deliberately excluded from workspace orchestration: they
// verify traffic metrics over time and only make sense for one service at a
// time, whereas a workspace deploy must halt the whole mesh on failure.
const (
	StrategyClassic   = "classic"
	StrategyBlueGreen = "blue-green"
	StrategyRolling   = "rolling"
)

// ServiceSpec is one service entry of the manifest, keyed by service name.
type ServiceSpec struct {
	// Path is the service directory, relative to the workspace root (or
	// absolute). It must exist at resolve time.
	Path string `yaml:"path"`
	// Port is the public port of the service. Zero means the port is
	// assigned automatically from the orchestration command's port base.
	Port int `yaml:"port,omitempty"`
	// Strategy selects the deploy strategy for the service: classic,
	// blue-green, or rolling. Empty defers to the service's own phelix.yaml,
	// then to its persisted deploy.json mode.
	Strategy string `yaml:"strategy,omitempty"`
	// Replicas configures rolling deploys. It is only meaningful together
	// with the rolling strategy.
	Replicas int `yaml:"replicas,omitempty"`
	// DependsOn lists service names that must deploy before this one.
	DependsOn []string `yaml:"depends_on,omitempty"`
}

// Manifest is the schema of phelix.workspace.yaml.
type Manifest struct {
	// Name labels the workspace in state, status output, and locks. Empty
	// defaults to the base name of the workspace root directory.
	Name string `yaml:"name,omitempty"`
	// Discover merges members discovered from a native go.work or Cargo
	// workspace into the manifest's explicit services. Explicit entries win
	// on name collisions; discovered members without a port get one assigned
	// at orchestration time. Without the flag the manifest is authoritative
	// and native files are ignored.
	Discover bool `yaml:"discover,omitempty"`
	// Services maps service names to their specs. At least one entry is
	// required unless Discover merges members in.
	Services map[string]*ServiceSpec `yaml:"services,omitempty"`
}

// ManifestPath returns the manifest path inside dir.
func ManifestPath(dir string) string {
	return filepath.Join(dir, FileName)
}

// ManifestExists reports whether a manifest is present in dir.
func ManifestExists(dir string) bool {
	_, err := os.Stat(ManifestPath(dir))
	return err == nil
}

// Load reads and validates phelix.workspace.yaml from dir. A missing file is
// reported as CodeNotFound so callers can fall back to native discovery. An
// invalid file is an error: a broken manifest must never silently degrade to
// a partial workspace.
func Load(dir string) (*Manifest, error) {
	path := ManifestPath(dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, phelixerr.Newf(phelixerr.CodeNotFound, "%s not found in %s", FileName, dir)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", path)
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "%s is malformed", path)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// validateServiceName applies the app-name rules to a workspace service,
// because every workspace service becomes a Phelix app: the name keys the
// per-app state directory, deploy.json, and versions.json.
func validateServiceName(name string) error {
	if err := deploy.ValidateAppName(name); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "invalid service name %q", name)
	}
	return nil
}

// Validate checks the manifest's internal consistency: service names, ports,
// strategies, replica configuration, and dependency references. With discover
// enabled a dependency may name a member that only discovery supplies, so the
// reference check waits for Resolve, which has the merged service set.
// Filesystem and graph checks belong to Resolve too.
func (m *Manifest) Validate() error {
	for name, spec := range m.Services {
		if err := validateServiceName(name); err != nil {
			return err
		}
		if spec == nil {
			return phelixerr.Newf(phelixerr.CodeConfiguration, "%s: service %q has no configuration", FileName, name)
		}
		if strings.TrimSpace(spec.Path) == "" {
			return phelixerr.Newf(phelixerr.CodeConfiguration, "%s: service %q is missing path", FileName, name)
		}
		if spec.Port != 0 {
			if err := phelixport.Validate(spec.Port); err != nil {
				return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "%s: service %q has an invalid port", FileName, name)
			}
		}
		switch spec.Strategy {
		case "", StrategyClassic, StrategyBlueGreen, StrategyRolling:
		default:
			return phelixerr.Newf(
				phelixerr.CodeConfiguration,
				"%s: service %q has invalid strategy %q\nHint: workspace strategies are classic, blue-green, rolling",
				FileName, name, spec.Strategy,
			)
		}
		if spec.Replicas < 0 {
			return phelixerr.Newf(phelixerr.CodeConfiguration, "%s: service %q has negative replicas", FileName, name)
		}
		if spec.Replicas > 0 && spec.Strategy != StrategyRolling {
			return phelixerr.Newf(
				phelixerr.CodeConfiguration,
				"%s: service %q sets replicas without the rolling strategy",
				FileName, name,
			)
		}
		for _, dep := range spec.DependsOn {
			if dep == name {
				return phelixerr.Newf(phelixerr.CodeConfiguration, "%s: service %q depends on itself", FileName, name)
			}
			if m.Discover {
				continue
			}
			if _, ok := m.Services[dep]; !ok {
				return phelixerr.Newf(
					phelixerr.CodeConfiguration,
					"%s: service %q depends on unknown service %q",
					FileName, name, dep,
				)
			}
		}
	}
	return nil
}
