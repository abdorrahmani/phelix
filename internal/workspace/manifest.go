package workspace

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"go.yaml.in/yaml/v3"
)

// Mode is a discovery toggle: auto (decide from what is present), on (require
// the source) or off (ignore it). An absent key defaults to ModeAuto.
type Mode string

const (
	ModeAuto Mode = "auto"
	ModeOn   Mode = "on"
	ModeOff  Mode = "off"
)

// Valid reports whether m is one of the three known modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeAuto, ModeOn, ModeOff:
		return true
	}
	return false
}

// Manifest is the parsed phelix.workspace.yaml document — a schema deliberately
// separate from project.Config (the single-app phelix.yaml). An empty Name or
// Root means the key was absent; Phase 1 defaults Name to the sanitized
// basename of the workspace root and Root to the manifest's directory.
type Manifest struct {
	Version   int                    `yaml:"version"`
	Name      string                 `yaml:"name,omitempty"`
	Root      string                 `yaml:"root,omitempty"`
	Discovery DiscoverySpec          `yaml:"discovery,omitempty"`
	Defaults  ServiceDefaults        `yaml:"defaults,omitempty"`
	Services  map[string]ServiceSpec `yaml:"services,omitempty"`
}

// DiscoverySpec controls auto-discovery of members from go.work and Cargo and
// optional glob filters over the discovered set (relative to the workspace
// root). Phase 1 implements the logic; Phase 0 only represents it.
type DiscoverySpec struct {
	GoWork  Mode     `yaml:"go_work,omitempty"`
	Cargo   Mode     `yaml:"cargo,omitempty"`
	Include []string `yaml:"include,omitempty"`
	Exclude []string `yaml:"exclude,omitempty"`
}

// ServiceDefaults is applied to every service unless the service overrides it.
type ServiceDefaults struct {
	Strategy string      `yaml:"strategy,omitempty"`
	Runtime  string      `yaml:"runtime,omitempty"`
	Replicas int         `yaml:"replicas,omitempty"`
	Health   *HealthSpec `yaml:"health,omitempty"`
}

// ServiceSpec is one service's manifest-level configuration. The map key in
// Manifest.Services is the canonical service name.
type ServiceSpec struct {
	// Path is the service directory relative to the workspace root. Required
	// when the service is not auto-discovered, or to disambiguate.
	Path string `yaml:"path,omitempty"`
	// Port is the public proxy port, unique across the mesh. It is distinct from
	// the per-slot internal instance port the deploy engine assigns and from the
	// PORT env var Phelix injects to tell an instance which internal port to bind.
	Port int `yaml:"port,omitempty"`
	// Strategy, Runtime, Replicas and Health override the manifest defaults;
	// empty values fall through the precedence chain (defaults → per-service
	// phelix.yaml → built-in) resolved in Phase 1.
	Strategy  string        `yaml:"strategy,omitempty"`
	Runtime   string        `yaml:"runtime,omitempty"`
	Replicas  int           `yaml:"replicas,omitempty"`
	DependsOn []string      `yaml:"depends_on,omitempty"`
	Health    *HealthSpec   `yaml:"health,omitempty"`
	Build     *ServiceBuild `yaml:"build,omitempty"`
}

// HealthSpec is the manifest's per-service health override: a single mode+path.
// It is a minimal local mirror, not project.HealthConfig — that type is a list
// of named endpoints (name/path/interval/retries/mode), which does not match
// the manifest's flat {mode, path}. Phase 1 resolves it against the service's
// own phelix.yaml health block.
type HealthSpec struct {
	Mode string `yaml:"mode,omitempty"`
	Path string `yaml:"path,omitempty"`
}

// ServiceBuild is passthrough build configuration for the per-service build.
type ServiceBuild struct {
	Args []string          `yaml:"args,omitempty"`
	Env  map[string]string `yaml:"env,omitempty"`
}

// validStrategies and validRuntimes mirror project.Strategy*/RuntimeNative/
// RuntimeDocker — the external-contract vocabulary a service's deploy
// understands. Inlined to keep this foundation package dependency-light.
//
// ponytail: duplicates internal/project's enum values; Phase 1 imports
// internal/project for precedence resolution and can converge them.
var validStrategies = map[string]bool{
	"classic": true, "blue-green": true, "rolling": true, "canary": true, "progressive": true,
}

var validRuntimes = map[string]bool{"native": true, "docker": true}

// ParseManifest strictly decodes a phelix.workspace.yaml document. Unknown keys
// are rejected (KnownFields) so typos surface loudly. It performs only
// well-formedness checks (version, enum values, YAML shape) and structural
// normalization; discovery and cross-field validation are later phases.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, phelixerr.New(phelixerr.CodeValidation, "workspace manifest is empty")
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeValidation, err, "parse workspace manifest")
	}
	if err := m.finalize(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadManifest reads manifestPath and parses it. A missing file is CodeNotFound
// so callers can tell "no workspace here" from "broken manifest".
func LoadManifest(manifestPath string) (*Manifest, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, phelixerr.Wrapf(phelixerr.CodeNotFound, err, "workspace manifest %s not found", manifestPath)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "read workspace manifest %s", manifestPath)
	}
	return ParseManifest(data)
}

// finalize applies Phase 0's well-formedness checks (version, enums) then
// structural normalization. It never inspects ports, on-disk paths, depends_on
// targets, uniqueness or cycles — those are Phase 1 validation.
func (m *Manifest) finalize() error {
	if m.Version == 0 {
		return phelixerr.New(phelixerr.CodeValidation,
			"workspace manifest is missing required field 'version' (expected 1)")
	}
	if m.Version != 1 {
		return phelixerr.Newf(phelixerr.CodeValidation,
			"unsupported workspace manifest version %d (this build supports version 1)", m.Version)
	}
	if m.Discovery.GoWork != "" && !m.Discovery.GoWork.Valid() {
		return invalidEnum("discovery.go_work", string(m.Discovery.GoWork), "auto, on, off")
	}
	if m.Discovery.Cargo != "" && !m.Discovery.Cargo.Valid() {
		return invalidEnum("discovery.cargo", string(m.Discovery.Cargo), "auto, on, off")
	}
	if err := checkStrategyRuntime("defaults", m.Defaults.Strategy, m.Defaults.Runtime); err != nil {
		return err
	}
	names := make([]string, 0, len(m.Services))
	for name := range m.Services {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic error order across runs
	for _, name := range names {
		svc := m.Services[name]
		if err := checkStrategyRuntime("services."+name, svc.Strategy, svc.Runtime); err != nil {
			return err
		}
	}
	m.normalize()
	return nil
}

func checkStrategyRuntime(ctx, strategy, runtime string) error {
	if strategy != "" && !validStrategies[strategy] {
		return invalidEnum(ctx+".strategy", strategy, "classic, blue-green, rolling, canary, progressive")
	}
	if runtime != "" && !validRuntimes[runtime] {
		return invalidEnum(ctx+".runtime", runtime, "native, docker")
	}
	return nil
}

func invalidEnum(key, got, want string) error {
	return phelixerr.Newf(phelixerr.CodeValidation, "invalid %s %q (expected one of: %s)", key, got, want)
}

// normalize applies structural defaults and slash-cleans paths, leaving empty
// optional fields zero so Phase 1 can tell "unset" from a real value.
func (m *Manifest) normalize() {
	m.Name = strings.TrimSpace(m.Name)
	m.Root = cleanRelPath(m.Root)
	if m.Discovery.GoWork == "" {
		m.Discovery.GoWork = ModeAuto
	}
	if m.Discovery.Cargo == "" {
		m.Discovery.Cargo = ModeAuto
	}
	for name, svc := range m.Services {
		svc.Path = cleanRelPath(svc.Path)
		m.Services[name] = svc
	}
}

// cleanRelPath normalizes a manifest path to a deterministic slash form
// (Windows separators folded to '/', '.'/'..' and trailing slashes collapsed).
// Empty stays empty — it is never turned into ".".
func cleanRelPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return path.Clean(strings.ReplaceAll(s, `\`, "/"))
}
