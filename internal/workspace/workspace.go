package workspace

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
)

// Service is one resolved member of a workspace: a directory Phelix can
// build and run, with an optional port, deploy strategy, and dependencies.
type Service struct {
	// Name is the manifest key or the discovered directory base name. It is
	// also the Phelix app name the service is registered under.
	Name string
	// Path is the absolute service directory.
	Path string
	// Port is the public port. Zero means it is assigned by AssignPorts
	// before orchestration, or derived from app state at status time.
	Port int
	// Strategy is classic, blue-green, or rolling. Empty defers to the
	// service's own configuration during deploy.
	Strategy string
	// Replicas configures rolling deploys. Zero defers to the service.
	Replicas int
	// DependsOn lists service names that must be deployed first.
	DependsOn []string
}

// Workspace is a fully resolved workspace: a root directory and the sorted
// set of services it defines, either through the manifest or through native
// discovery.
type Workspace struct {
	Name string
	// Root is the absolute directory the manifest or workspace file lives in.
	Root string
	// Source records where the definition came from: manifest, go.work, or
	// cargo.
	Source   string
	Services []*Service
}

// Service returns the named service, or nil when the workspace has none.
func (w *Workspace) Service(name string) *Service {
	for _, svc := range w.Services {
		if svc.Name == name {
			return svc
		}
	}
	return nil
}

// Names returns the sorted service names.
func (w *Workspace) Names() []string {
	names := make([]string, len(w.Services))
	for i, svc := range w.Services {
		names[i] = svc.Name
	}
	return names
}

// Deps maps every service name to its declared dependencies, the input shape
// of the graph functions.
func (w *Workspace) Deps() map[string][]string {
	deps := make(map[string][]string, len(w.Services))
	for _, svc := range w.Services {
		deps[svc.Name] = append([]string(nil), svc.DependsOn...)
	}
	return deps
}

// TopoLevels returns the dependency levels of the workspace, validating the
// graph every call.
func (w *Workspace) TopoLevels() ([][]string, error) {
	return TopoLevels(w.Names(), w.Deps())
}

// TopoOrder returns the flattened dependency order of the workspace.
func (w *Workspace) TopoOrder() ([]string, error) {
	return TopoOrder(w.Names(), w.Deps())
}

// Resolve builds a workspace from root. A phelix.workspace.yaml manifest is
// authoritative when present (with discovery merging in when the manifest
// sets discover: true); otherwise native go.work or Cargo discovery defines
// the services. The returned workspace is validated: paths exist inside the
// root, dependency references are known, and the dependency graph is
// acyclic. Ports are assigned separately with AssignPorts so status can
// resolve a workspace without reserving anything.
func Resolve(root string) (*Workspace, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "could not resolve workspace root %q", root)
	}
	info, err := os.Stat(absRoot)
	if err != nil || !info.IsDir() {
		return nil, phelixerr.Newf(phelixerr.CodeNotFound, "workspace root %q is not a directory", root)
	}

	w := &Workspace{Root: absRoot}
	if ManifestExists(absRoot) {
		w.Source = SourceManifest
		manifest, merr := Load(absRoot)
		if merr != nil {
			return nil, merr
		}
		if manifest.Name != "" {
			w.Name = manifest.Name
		}
		if err := resolveManifestServices(w, manifest); err != nil {
			return nil, err
		}
		if manifest.Discover {
			if err := mergeDiscoveredServices(w); err != nil {
				return nil, err
			}
		}
	} else {
		discovery, derr := Discover(absRoot)
		if derr != nil {
			return nil, derr
		}
		w.Source = discovery.Source
		if err := addDiscoveredMembers(w, discovery.Members); err != nil {
			return nil, err
		}
	}

	if w.Name == "" {
		w.Name = filepath.Base(absRoot)
	}
	if err := validateServiceName(w.Name); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "workspace name %q is not a valid identifier", w.Name)
	}

	if len(w.Services) == 0 {
		return nil, phelixerr.Newf(
			phelixerr.CodeValidation,
			"workspace %q in %s defines no runnable services",
			w.Name, absRoot,
		)
	}

	if clash := duplicatePort(w.Services); clash != "" {
		return nil, phelixerr.Newf(phelixerr.CodeValidation, "duplicate port in %s: %s", FileName, clash)
	}
	if _, err := w.TopoLevels(); err != nil {
		return nil, err
	}
	return w, nil
}

// resolveManifestServices turns manifest entries into services, validating
// that every path exists below the workspace root.
func resolveManifestServices(w *Workspace, manifest *Manifest) error {
	for _, name := range sortedManifestKeys(manifest.Services) {
		spec := manifest.Services[name]
		path, err := resolveServicePath(w.Root, name, spec.Path)
		if err != nil {
			return err
		}
		w.Services = append(w.Services, &Service{
			Name:      name,
			Path:      path,
			Port:      spec.Port,
			Strategy:  spec.Strategy,
			Replicas:  spec.Replicas,
			DependsOn: append([]string(nil), spec.DependsOn...),
		})
	}
	return nil
}

// resolveServicePath resolves one manifest path against the workspace root,
// rejecting paths that point outside it and directories that do not exist.
// Absolute paths are accepted and used as-is.
func resolveServicePath(root, name, declared string) (string, error) {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return "", phelixerr.Newf(phelixerr.CodeConfiguration, "%s: service %q is missing path", FileName, name)
	}
	var full string
	if filepath.IsAbs(declared) {
		full = filepath.Clean(declared)
	} else {
		full = filepath.Join(root, filepath.FromSlash(filepath.Clean(declared)))
		rel, relErr := filepath.Rel(root, full)
		if relErr != nil || rel == ".." || filepath.IsAbs(rel) || hasDotDotPrefix(rel) {
			return "", phelixerr.Newf(
				phelixerr.CodeConfiguration,
				"%s: service %q path %q escapes the workspace root",
				FileName, name, declared,
			)
		}
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", phelixerr.Newf(
			phelixerr.CodeConfiguration,
			"%s: service %q path %q does not exist in the workspace",
			FileName, name, declared,
		)
	}
	if !info.IsDir() {
		return "", phelixerr.Newf(phelixerr.CodeConfiguration, "%s: service %q path %q is not a directory", FileName, name, declared)
	}
	return full, nil
}

// mergeDiscoveredServices adds runnable native-workspace members that the
// manifest does not already name. Explicit manifest entries win: a
// discovered member whose base name collides with a manifest service is
// skipped because the manifest defines that service's directory and port.
func mergeDiscoveredServices(w *Workspace) error {
	discovery, err := Discover(w.Root)
	if err != nil {
		return err
	}
	return addDiscoveredMembers(w, discovery.Members)
}

// addDiscoveredMembers appends runnable members not already present,
// keeping the service list sorted by name.
func addDiscoveredMembers(w *Workspace, members []string) error {
	runnable, err := DiscoverRunnables(w.Root, members)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for _, svc := range w.Services {
		existing[svc.Name] = true
	}
	var names []string
	for name := range runnable {
		if !existing[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path, err := resolveServicePath(w.Root, name, runnable[name])
		if err != nil {
			return err
		}
		w.Services = append(w.Services, &Service{
			Name: name,
			Path: path,
		})
	}
	sort.Slice(w.Services, func(i, j int) bool { return w.Services[i].Name < w.Services[j].Name })
	return nil
}

// AssignPorts gives every service without an explicit port a public port,
// walking up from base and skipping ports the workspace already uses. The
// base itself is validated like any other port.
func AssignPorts(w *Workspace, base int) error {
	if err := phelixport.Validate(base); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "invalid workspace port base %d", base)
	}
	used := map[int]bool{}
	for _, svc := range w.Services {
		if svc.Port != 0 {
			used[svc.Port] = true
		}
	}
	var unassigned []*Service
	for _, svc := range w.Services {
		if svc.Port == 0 {
			unassigned = append(unassigned, svc)
		}
	}
	sort.Slice(unassigned, func(i, j int) bool { return unassigned[i].Name < unassigned[j].Name })
	candidate := base
	for _, svc := range unassigned {
		for used[candidate] {
			candidate++
			if candidate > 65535 {
				return phelixerr.Newf(
					phelixerr.CodeValidation,
					"workspace ran out of ports above base %d while assigning %q",
					base, svc.Name,
				)
			}
		}
		svc.Port = candidate
		used[candidate] = true
	}
	return nil
}

func sortedManifestKeys(services map[string]*ServiceSpec) []string {
	keys := make([]string, 0, len(services))
	for name := range services {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

func hasDotDotPrefix(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
