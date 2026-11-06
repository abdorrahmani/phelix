package security

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/workspace"
)

// InventoryRust inventories the Rust dependencies of dir. Cargo.lock is
// the resolution record: registry crates carry fully resolved versions.
// Without a lockfile the inventory degrades to manifest requirements.
func InventoryRust(dir string) (*Inventory, error) {
	root, err := loadCargoManifest(dir)
	if err != nil {
		return nil, err
	}

	memberRels, err := cargoMemberDirs(dir, root)
	if err != nil {
		return nil, err
	}

	direct := map[string]*cargoDirectInfo{}
	memberCrateNames := map[string]bool{}
	for _, rel := range memberRels {
		memberDir := filepath.Join(dir, filepath.FromSlash(rel))
		manifest, err := loadCargoManifest(memberDir)
		if err != nil {
			return nil, err
		}
		name := rel
		if manifest.Package != nil && manifest.Package.Name != "" {
			name = manifest.Package.Name
		}
		memberCrateNames[name] = true
		deps, err := resolveMemberDeps(manifest, name, workspaceDeps(root))
		if err != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeValidation, err, "Cargo.toml in %s declares unusable dependencies", rel)
		}
		mergeDirectMaps(direct, deps)
	}

	lock, lockErr := loadCargoLock(dir)
	if lockErr != nil {
		return nil, lockErr
	}
	if lock == nil {
		return rustManifestOnlyInventory(direct), nil
	}
	return rustLockInventory(direct, memberCrateNames, lock)
}

// cargoMemberDirs returns the workspace member directories relative to
// dir. A manifest without a [workspace] table scans as a single crate.
func cargoMemberDirs(dir string, root *cargoManifestFile) ([]string, error) {
	if root.Workspace == nil {
		return []string{"."}, nil
	}
	discovery, err := workspace.Discover(dir)
	if err != nil {
		return nil, err
	}
	if discovery.Source != workspace.SourceCargo {
		return nil, phelixerr.New(phelixerr.CodeUnsupportedProject,
			"workspace discovery resolved a Go workspace; scan the Rust workspace directory directly")
	}
	return discovery.Members, nil
}

func workspaceDeps(root *cargoManifestFile) map[string]*cargoDepRaw {
	if root == nil || root.Workspace == nil {
		return nil
	}
	return root.Workspace.Dependencies
}

type cargoLockFile struct {
	Version int                `toml:"version"`
	Package []cargoLockPackage `toml:"package"`
}

type cargoLockPackage struct {
	Name         string   `toml:"name"`
	Version      string   `toml:"version"`
	Source       string   `toml:"source"`
	Dependencies []string `toml:"dependencies"`
}

// loadCargoLock parses dir/Cargo.lock. A missing lockfile returns (nil,
// nil): the caller degrades to a manifest-only inventory. A malformed
// lockfile is an error; half of a broken resolution record must never be
// trusted.
func loadCargoLock(dir string) (*cargoLockFile, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "Cargo.lock"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", filepath.Join(dir, "Cargo.lock"))
	}
	var lock cargoLockFile
	if err := toml.Unmarshal(raw, &lock); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeValidation, err, "Cargo.lock is malformed")
	}
	for i, pkg := range lock.Package {
		if pkg.Name == "" || pkg.Version == "" {
			return nil, phelixerr.Newf(phelixerr.CodeValidation, "Cargo.lock package entry %d is missing a name or version", i)
		}
	}
	return &lock, nil
}

// rustLockInventory builds the inventory from a parsed Cargo.lock.
func rustLockInventory(direct map[string]*cargoDirectInfo, memberCrateNames map[string]bool, lock *cargoLockFile) (*Inventory, error) {
	locked := map[string][]int{}
	for i, pkg := range lock.Package {
		locked[pkg.Name] = append(locked[pkg.Name], i)
	}

	reachable := cargoLockReachable(lock, locked, memberCrateNames)

	modules := []Module{}
	for i, pkg := range lock.Package {
		if pkg.Source == "" && memberCrateNames[pkg.Name] {
			continue
		}
		kind, matchable := cargoSourceKind(pkg.Source)
		mod := Module{
			Name:         pkg.Name,
			Version:      pkg.Version,
			Resolved:     true,
			ResolvedFrom: "cargo.lock",
			SourceKind:   kind,
			SourceDetail: pkg.Source,
			Matchable:    matchable,
			Reachable:    reachable[i],
		}
		switch kind {
		case SourceRegistry:
			mod.Confidence = ConfidenceResolved
		case SourceGit:
			mod.Confidence = ConfidenceResolved
			mod.Notes = append(mod.Notes, "git dependency; registry advisories are not evaluated")
		case SourcePath:
			mod.Confidence = ConfidenceResolved
			mod.Matchable = false
			mod.Notes = append(mod.Notes, "path dependency; registry advisories are not evaluated")
		case SourceOtherRegistry:
			mod.Confidence = ConfidenceResolved
			mod.Notes = append(mod.Notes, "non-crates.io registry; not matched against public advisories")
		default:
			mod.Confidence = ConfidenceUnknown
			mod.Matchable = false
			mod.Notes = append(mod.Notes, "unrecognized source identity; not matched against public advisories")
		}
		if info := direct[pkg.Name]; info != nil && cargoDirectEntry(info, pkg.Version) {
			mod.Direct = true
			mod.Optional = info.Optional
			mod.Dev = info.Dev
			mod.DeclaredBy = info.Members
			if info.Git {
				mod.Notes = append(mod.Notes, "declared as a git dependency in a workspace manifest")
			}
			if info.Path {
				mod.Notes = append(mod.Notes, "declared as a path dependency in a workspace manifest")
			}
		}
		if !mod.Reachable {
			mod.Notes = append(mod.Notes, "not reachable from any workspace member in the lockfile dependency graph")
		}
		modules = append(modules, mod)
	}

	sort.Slice(modules, func(i, j int) bool {
		if modules[i].Name != modules[j].Name {
			return modules[i].Name < modules[j].Name
		}
		return modules[i].Version < modules[j].Version
	})

	inv := &Inventory{
		Ecosystem: EcosystemRust,
		Level:     InventoryResolved,
		Modules:   modules,
	}
	inv.Notes = append(inv.Notes, "versions come from Cargo.lock, the resolved record of what a build compiles")
	return inv, nil
}

// rustManifestOnlyInventory builds the degraded inventory used when
// Cargo.lock is missing. Requirements are never presented as resolved
// versions.
func rustManifestOnlyInventory(direct map[string]*cargoDirectInfo) *Inventory {
	modules := []Module{}
	for name, info := range direct {
		mod := Module{
			Name:       name,
			Direct:     true,
			Dev:        info.Dev,
			Optional:   info.Optional,
			DeclaredBy: info.Members,
			Reachable:  true,
		}
		switch {
		case info.Git:
			mod.SourceKind = SourceGit
			mod.Confidence = ConfidenceUnknown
			mod.Matchable = false
			mod.Notes = append(mod.Notes, "git dependency; registry advisories are not evaluated")
		case info.Path:
			mod.SourceKind = SourcePath
			mod.Confidence = ConfidenceUnknown
			mod.Matchable = false
			mod.Notes = append(mod.Notes, "path dependency; registry advisories are not evaluated")
		default:
			mod.SourceKind = SourceRegistry
			mod.Confidence = ConfidenceRequirement
			mod.Requirement = info.Requirement
			mod.Matchable = info.Requirement != ""
		}
		if mod.Requirement == "" && mod.Matchable {
			mod.Matchable = false
			mod.Notes = append(mod.Notes, "no version requirement; nothing to evaluate")
		}
		modules = append(modules, mod)
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Name < modules[j].Name })

	return &Inventory{
		Ecosystem: EcosystemRust,
		Level:     InventoryManifest,
		Modules:   modules,
		Notes: []string{
			"Cargo.lock is missing; versions are manifest requirements, not resolved packages",
			"requirement ranges are evaluated as intervals; the selected version is unknown",
		},
	}
}

// cargoDirectEntry decides whether one lockfile entry is the resolution
// of a manifest declaration. The manifest requirement picks the entry
// when it parses; otherwise every entry of that crate name is treated as
// declared rather than silently dropping one.
func cargoDirectEntry(info *cargoDirectInfo, version string) bool {
	if info == nil {
		return false
	}
	if info.Requirement == "" {
		return true
	}
	req, err := parseCargoRequirement(info.Requirement)
	if err != nil {
		return true
	}
	v, ok := parseCargoVersion(version)
	if !ok {
		return true
	}
	return cargoVersionInInterval(v, req)
}

// cargoSourceKind classifies a Cargo.lock source string. Only the
// crates.io index identities are matchable against public advisories.
func cargoSourceKind(source string) (SourceKind, bool) {
	switch {
	case source == "registry+https://github.com/rust-lang/crates.io-index",
		source == "sparse+https://index.crates.io/":
		return SourceRegistry, true
	case strings.HasPrefix(source, "registry+"), strings.HasPrefix(source, "sparse+"):
		return SourceOtherRegistry, false
	case strings.HasPrefix(source, "git+"):
		return SourceGit, false
	case source == "":
		return SourcePath, false
	default:
		return SourceKind("unknown"), false
	}
}

// cargoLockReachable computes which lockfile entries the dependency graph
// connects to a workspace member, following the edges recorded on each
// package entry.
func cargoLockReachable(lock *cargoLockFile, locked map[string][]int, memberCrateNames map[string]bool) []bool {
	reachable := make([]bool, len(lock.Package))
	var visit func(index int)
	visit = func(index int) {
		if reachable[index] {
			return
		}
		reachable[index] = true
		for _, edge := range lock.Package[index].Dependencies {
			for _, target := range cargoEdgeTargets(edge, lock, locked) {
				visit(target)
			}
		}
	}
	for i, pkg := range lock.Package {
		if pkg.Source == "" && memberCrateNames[pkg.Name] {
			visit(i)
		}
	}
	return reachable
}

// cargoEdgeTargets resolves one lockfile dependency edge reference, which
// may be "name", "name version", or "name version (source)", to the
// package entries it refers to.
func cargoEdgeTargets(edge string, lock *cargoLockFile, locked map[string][]int) []int {
	fields := strings.Fields(edge)
	if len(fields) == 0 {
		return nil
	}
	candidates := locked[fields[0]]
	if len(fields) == 1 || len(candidates) <= 1 {
		return candidates
	}
	var version, source string
	if strings.Contains(fields[1], ".") {
		version = fields[1]
	}
	if len(fields) >= 3 {
		source = strings.Trim(fields[2], "()")
	}
	var out []int
	for _, index := range candidates {
		pkg := lock.Package[index]
		if version != "" && pkg.Version != version {
			continue
		}
		if source != "" && pkg.Source != source {
			continue
		}
		out = append(out, index)
	}
	if len(out) == 0 {
		return candidates
	}
	return out
}
