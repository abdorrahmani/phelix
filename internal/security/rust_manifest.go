package security

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// cargoDepRaw is one dependency declaration from a Cargo.toml. A
// declaration is either a bare version requirement string or a table, so
// dependencies decode as untyped values and convert explicitly.
type cargoDepRaw struct {
	Key         string
	Requirement string
	HasVersion  bool
	Rename      string
	Workspace   bool
	Optional    bool
	Git         string
	Path        string
}

// cargoDepFromValue converts one dependency value: a string is a version
// requirement, a table is a dependency specification.
func cargoDepFromValue(key string, value any) (*cargoDepRaw, error) {
	if req, ok := value.(string); ok {
		return &cargoDepRaw{Key: key, Requirement: req, HasVersion: true}, nil
	}
	table, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("dependency %q is neither a version string nor a table", key)
	}
	dep := &cargoDepRaw{Key: key}
	str := func(field string) (string, bool, error) {
		v, present := table[field]
		if !present || v == nil {
			return "", false, nil
		}
		s, ok := v.(string)
		if !ok {
			return "", false, fmt.Errorf("dependency %q field %q must be a string", key, field)
		}
		return s, true, nil
	}
	if req, present, err := str("version"); err != nil {
		return nil, err
	} else if present {
		dep.Requirement, dep.HasVersion = req, true
	}
	if rename, present, err := str("package"); err != nil {
		return nil, err
	} else if present {
		dep.Rename = rename
	}
	if git, present, err := str("git"); err != nil {
		return nil, err
	} else if present {
		dep.Git = git
	}
	if path, present, err := str("path"); err != nil {
		return nil, err
	} else if present {
		dep.Path = path
	}
	boolean := func(field string) (bool, error) {
		v, present := table[field]
		if !present || v == nil {
			return false, nil
		}
		bv, ok := v.(bool)
		if !ok {
			return false, fmt.Errorf("dependency %q field %q must be a boolean", key, field)
		}
		return bv, nil
	}
	if workspace, err := boolean("workspace"); err != nil {
		return nil, err
	} else {
		dep.Workspace = workspace
	}
	if optional, err := boolean("optional"); err != nil {
		return nil, err
	} else {
		dep.Optional = optional
	}
	return dep, nil
}

// cargoDepTable converts a dependency table from an untyped decode.
func cargoDepTable(value any) (map[string]*cargoDepRaw, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("dependency table is not a table")
	}
	out := make(map[string]*cargoDepRaw, len(raw))
	for key, v := range raw {
		dep, err := cargoDepFromValue(key, v)
		if err != nil {
			return nil, err
		}
		out[key] = dep
	}
	return out, nil
}

type cargoPackageSection struct {
	Name    string
	Version string
}

type cargoWorkspaceSection struct {
	Members        []string
	DefaultMembers []string
	Dependencies   map[string]*cargoDepRaw
}

type cargoTargetSection struct {
	Dependencies      map[string]*cargoDepRaw
	DevDependencies   map[string]*cargoDepRaw
	BuildDependencies map[string]*cargoDepRaw
}

type cargoManifestFile struct {
	Package           *cargoPackageSection
	Workspace         *cargoWorkspaceSection
	Dependencies      map[string]*cargoDepRaw
	DevDependencies   map[string]*cargoDepRaw
	BuildDependencies map[string]*cargoDepRaw
	Target            map[string]*cargoTargetSection
}

// loadCargoManifest parses the Cargo.toml in dir. Dependency tables are
// decoded as untyped values and converted explicitly so both the string
// and table declaration forms are accepted.
func loadCargoManifest(dir string) (*cargoManifestFile, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", filepath.Join(dir, "Cargo.toml"))
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeValidation, err, "Cargo.toml in %s is malformed", filepath.Base(dir))
	}
	return cargoManifestFromDoc(doc)
}

func cargoManifestFromDoc(doc map[string]any) (*cargoManifestFile, error) {
	file := &cargoManifestFile{}
	if pkg, ok := doc["package"].(map[string]any); ok {
		file.Package = &cargoPackageSection{}
		if name, ok := pkg["name"].(string); ok {
			file.Package.Name = name
		}
		if version, ok := pkg["version"].(string); ok {
			file.Package.Version = version
		}
	}
	if ws, ok := doc["workspace"].(map[string]any); ok {
		file.Workspace = &cargoWorkspaceSection{}
		if members, ok := ws["members"].([]any); ok {
			file.Workspace.Members = cargoStringList(members)
		}
		if members, ok := ws["default-members"].([]any); ok {
			file.Workspace.DefaultMembers = cargoStringList(members)
		}
		if depsAny, ok := ws["dependencies"]; ok {
			deps, err := cargoDepTable(depsAny)
			if err != nil {
				return nil, fmt.Errorf("[workspace.dependencies]: %w", err)
			}
			file.Workspace.Dependencies = deps
		}
	}
	tables := []struct {
		field string
		dst   *map[string]*cargoDepRaw
	}{
		{"dependencies", &file.Dependencies},
		{"dev-dependencies", &file.DevDependencies},
		{"build-dependencies", &file.BuildDependencies},
	}
	for _, t := range tables {
		value, ok := doc[t.field]
		if !ok {
			continue
		}
		deps, err := cargoDepTable(value)
		if err != nil {
			return nil, fmt.Errorf("[%s]: %w", t.field, err)
		}
		*t.dst = deps
	}
	if targets, ok := doc["target"].(map[string]any); ok {
		file.Target = map[string]*cargoTargetSection{}
		for name, value := range targets {
			table, ok := value.(map[string]any)
			if !ok {
				continue
			}
			section := &cargoTargetSection{}
			for _, t := range []struct {
				field string
				dst   *map[string]*cargoDepRaw
			}{
				{"dependencies", &section.Dependencies},
				{"dev-dependencies", &section.DevDependencies},
				{"build-dependencies", &section.BuildDependencies},
			} {
				value, ok := table[t.field]
				if !ok {
					continue
				}
				deps, err := cargoDepTable(value)
				if err != nil {
					return nil, fmt.Errorf("[target.%s.%s]: %w", name, t.field, err)
				}
				*t.dst = deps
			}
			file.Target[name] = section
		}
	}
	return file, nil
}

func cargoStringList(values []any) []string {
	var out []string
	for _, v := range values {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// cargoDirectInfo records how workspace manifests declare one crate.
type cargoDirectInfo struct {
	Name        string
	Requirement string
	Optional    bool
	Dev         bool
	Git         bool
	Path        bool
	Members     []string
}

// cargoDepCollector folds dependency declarations into one map keyed by
// real crate name, resolving workspace inheritance and package renames.
type cargoDepCollector struct {
	memberName    string
	workspaceDeps map[string]*cargoDepRaw
	out           map[string]*cargoDirectInfo
}

func (c *cargoDepCollector) add(kind string, key string, spec *cargoDepRaw) error {
	decl := spec
	if spec == nil {
		return nil
	}
	if spec.Workspace {
		decl = c.workspaceDeps[key]
		if decl == nil {
			return fmt.Errorf("dependency %q inherits from the workspace but [workspace.dependencies] does not declare it", key)
		}
	}
	name := decl.Rename
	if name == "" {
		name = key
	}
	info := c.out[name]
	if info == nil {
		info = &cargoDirectInfo{Name: name}
		c.out[name] = info
	}
	if decl.HasVersion && info.Requirement == "" {
		info.Requirement = decl.Requirement
	}
	info.Optional = info.Optional || decl.Optional
	if kind == "dev" {
		info.Dev = true
	}
	info.Git = info.Git || decl.Git != ""
	info.Path = info.Path || decl.Path != ""
	info.Members = appendUnique(info.Members, c.memberName)
	return nil
}

func (c *cargoDepCollector) addTable(kind string, table map[string]*cargoDepRaw) error {
	for key, spec := range table {
		if err := c.add(kind, key, spec); err != nil {
			return err
		}
	}
	return nil
}

// resolveMemberDeps collects every crate a member manifest declares,
// including target-specific tables. dev marks development-only
// declarations; build dependencies count as direct.
func resolveMemberDeps(member *cargoManifestFile, memberName string, workspaceDeps map[string]*cargoDepRaw) (map[string]*cargoDirectInfo, error) {
	c := &cargoDepCollector{memberName: memberName, workspaceDeps: workspaceDeps, out: map[string]*cargoDirectInfo{}}
	passes := []struct {
		kind  string
		table map[string]*cargoDepRaw
	}{
		{"normal", member.Dependencies},
		{"dev", member.DevDependencies},
		{"build", member.BuildDependencies},
	}
	for _, p := range passes {
		if err := c.addTable(p.kind, p.table); err != nil {
			return nil, err
		}
	}
	for _, target := range member.Target {
		if err := c.addTable("normal", target.Dependencies); err != nil {
			return nil, err
		}
		if err := c.addTable("dev", target.DevDependencies); err != nil {
			return nil, err
		}
		if err := c.addTable("build", target.BuildDependencies); err != nil {
			return nil, err
		}
	}
	return c.out, nil
}

// mergeDirectMaps folds member declarations into one map. A crate
// declared as a normal dependency anywhere is not development-only.
func mergeDirectMaps(dst, src map[string]*cargoDirectInfo) {
	for name, info := range src {
		existing := dst[name]
		if existing == nil {
			copied := *info
			dst[name] = &copied
			continue
		}
		existing.Dev = existing.Dev && info.Dev
		existing.Optional = existing.Optional || info.Optional
		existing.Git = existing.Git || info.Git
		existing.Path = existing.Path || info.Path
		if existing.Requirement == "" {
			existing.Requirement = info.Requirement
		}
		for _, member := range info.Members {
			existing.Members = appendUnique(existing.Members, member)
		}
	}
}

func appendUnique(values []string, next string) []string {
	for _, v := range values {
		if v == next {
			return values
		}
	}
	return append(values, next)
}
