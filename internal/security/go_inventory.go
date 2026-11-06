package security

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// InventoryGo inventories the Go dependencies of dir. It parses go.mod,
// go.work, and go.sum with golang.org/x/mod. It never invokes the Go
// toolchain, so the MVS build list is not resolved: requirements are
// recorded as declared minimums, go.sum entries as observed evidence, and
// version-pinned replace directives as the only fully resolved identities.
func InventoryGo(dir string) (*Inventory, error) {
	ws, err := loadGoWorkspace(dir)
	if err != nil {
		return nil, err
	}

	accs := map[string]*goModuleAcc{}
	memberPaths := map[string]bool{}
	replacements := map[string]*modfile.Replace{}
	conflictedReplacements := map[string]bool{}
	var order []string
	excludeCount := 0

	for _, member := range ws.members {
		raw, err := os.ReadFile(filepath.Join(member.dir, "go.mod"))
		if err != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", filepath.Join(member.rel, "go.mod"))
		}
		mod, err := modfile.Parse(filepath.Join(member.rel, "go.mod"), raw, nil)
		if err != nil {
			return nil, phelixerr.Wrapf(phelixerr.CodeValidation, err, "%s is malformed", filepath.Join(member.rel, "go.mod"))
		}
		if mod.Module == nil || mod.Module.Mod.Path == "" {
			return nil, phelixerr.Newf(phelixerr.CodeValidation, "%s has no module directive", filepath.Join(member.rel, "go.mod"))
		}
		memberPaths[mod.Module.Mod.Path] = true
		excludeCount += len(mod.Exclude)

		for _, req := range mod.Require {
			acc := accs[req.Mod.Path]
			if acc == nil {
				acc = &goModuleAcc{}
				accs[req.Mod.Path] = acc
				order = append(order, req.Mod.Path)
			}
			acc.declared = true
			if !req.Indirect {
				acc.direct = true
			}
			acc.declaredBy = append(acc.declaredBy, mod.Module.Mod.Path)
			acc.declaredVersion = goMaxVersion(acc.declaredVersion, req.Mod.Version)
		}

		var memberReplaces []*modfile.Replace
		memberReplaces = append(memberReplaces, ws.replaces...)
		memberReplaces = append(memberReplaces, mod.Replace...)
		for _, rep := range memberReplaces {
			if prev, ok := replacements[rep.Old.Path]; ok && !goSameReplacement(prev, rep) {
				conflictedReplacements[rep.Old.Path] = true
				continue
			}
			replacements[rep.Old.Path] = rep
		}
	}

	observedOnly := goAttachSumEvidence(ws.members, accs, memberPaths)

	modules := make([]Module, 0, len(accs))
	for _, path := range order {
		if memberPaths[path] {
			continue
		}
		acc := accs[path]
		acc.observed = dedupeSorted(acc.observed)
		mod := goModuleFromAcc(path, acc, replacements[path], conflictedReplacements[path], memberPaths)
		modules = append(modules, mod)
	}

	modules = append(modules, observedOnly...)
	sort.Slice(modules, func(i, j int) bool { return modules[i].Name < modules[j].Name })

	inv := &Inventory{
		Ecosystem: EcosystemGo,
		Level:     InventoryManifest,
		Modules:   modules,
	}
	inv.Notes = append(inv.Notes,
		"go.mod versions are build-list minimums; the selected version is not established without invoking the Go toolchain",
		"go.sum supplies observed versions as evidence; it is not proof of the selected build-list version",
	)
	if ws.workspace {
		inv.Notes = append(inv.Notes, "go.work workspace: requirements from member modules are merged per module path as a lower bound")
	}
	if excludeCount > 0 {
		inv.Notes = append(inv.Notes, "exclude directives are recorded but not applied offline")
	}
	return inv, nil
}

// goModuleFromAcc materializes one inventory module from its accumulated
// evidence and any applicable replace directive.
func goModuleFromAcc(path string, acc *goModuleAcc, rep *modfile.Replace, conflicted bool, memberPaths map[string]bool) Module {
	mod := Module{
		Name:             path,
		Direct:           acc.direct,
		DeclaredBy:       acc.declaredBy,
		Matchable:        true,
		Reachable:        true,
		SourceKind:       SourceGoModule,
		Confidence:       ConfidenceDeclaredMinimum,
		Notes:            acc.notes,
		Version:          acc.declaredVersion,
		ObservedVersions: acc.observed,
	}

	switch {
	case conflicted:
		mod.Matchable = false
		mod.Confidence = ConfidenceUnknown
		mod.Notes = append(mod.Notes, "conflicting replace directives across workspace members; identity left unresolved")
	case rep != nil && goReplaceApplies(rep, acc.declaredVersion):
		mod = goReplacedModule(mod, rep, memberPaths)
		mod.ObservedVersions = nil
	}

	if len(mod.ObservedVersions) == 1 && mod.Version == "" {
		mod.Version = mod.ObservedVersions[0]
	}
	return mod
}

// goReplacedModule rewrites a module entry according to its replace
// directive. A pinned replacement to a non-member path is the only Go
// identity offline scanning can call fully resolved.
func goReplacedModule(mod Module, rep *modfile.Replace, memberPaths map[string]bool) Module {
	mod.ReplacedFrom = rep.Old.Path
	mod.Name = rep.New.Path
	switch {
	case rep.New.Version == "":
		mod.Version = ""
		mod.Resolved = false
		mod.Confidence = ConfidenceUnknown
		mod.Matchable = false
		mod.Notes = append(mod.Notes, "replaced without a pinned version; selected version not established offline")
	case memberPaths[rep.New.Path]:
		mod.Version = rep.New.Version
		mod.Resolved = true
		mod.ResolvedFrom = "replace directive"
		mod.Confidence = ConfidenceResolved
		mod.SourceKind = SourceReplacePinned
		mod.Matchable = false
		mod.Notes = append(mod.Notes, "replaced by workspace member "+rep.New.Path+"; not matched against public advisories")
	default:
		mod.Version = rep.New.Version
		mod.Resolved = true
		mod.ResolvedFrom = "replace directive"
		mod.Confidence = ConfidenceResolved
		mod.SourceKind = SourceReplacePinned
	}
	return mod
}

// goReplaceApplies reports whether a replace directive governs a
// requirement with the given declared version. Unconstrained replaces
// apply to every requirement on the path.
func goReplaceApplies(rep *modfile.Replace, declaredVersion string) bool {
	return rep.Old.Version == "" || rep.Old.Version == declaredVersion
}

type goMember struct {
	dir string
	rel string
}

type goWorkspace struct {
	members   []goMember
	replaces  []*modfile.Replace
	workspace bool
}

// loadGoWorkspace returns the Go module roots to scan: every go.work
// member when a workspace file is present, otherwise dir itself.
func loadGoWorkspace(dir string) (*goWorkspace, error) {
	if !fileExists(filepath.Join(dir, "go.work")) {
		if !fileExists(filepath.Join(dir, "go.mod")) {
			return nil, phelixerr.Newf(phelixerr.CodeNotFound, "no go.mod or go.work found in %s", filepath.Base(dir))
		}
		return &goWorkspace{members: []goMember{{dir: dir, rel: "."}}}, nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, "go.work"))
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read go.work")
	}
	work, err := modfile.ParseWork(filepath.Join(dir, "go.work"), raw, nil)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeValidation, err, "go.work is malformed")
	}
	ws := &goWorkspace{workspace: true, replaces: work.Replace}
	for _, use := range work.Use {
		rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(use.Path)), "./")
		ws.members = append(ws.members, goMember{dir: filepath.Join(dir, filepath.FromSlash(rel)), rel: rel})
	}
	if len(ws.members) == 0 {
		return nil, phelixerr.New(phelixerr.CodeValidation, "go.work declares no modules")
	}
	return ws, nil
}

// goModuleAcc accumulates evidence for one module path across members.
type goModuleAcc struct {
	declared        bool
	declaredVersion string
	direct          bool
	declaredBy      []string
	observed        []string
	notes           []string
}

// goSameReplacement reports whether two replace directives redirect the
// same original module to the same target.
func goSameReplacement(a, b *modfile.Replace) bool {
	return a.Old.Path == b.Old.Path && a.Old.Version == b.Old.Version &&
		a.New.Path == b.New.Path && a.New.Version == b.New.Version
}

// goMaxVersion keeps the higher of two declared requirement versions so a
// workspace-wide lower bound never under-states any member's requirement.
func goMaxVersion(current, next string) string {
	if next == "" {
		return current
	}
	if current == "" {
		return next
	}
	a, b := goSemver(current), goSemver(next)
	if !semver.IsValid(a) || !semver.IsValid(b) {
		return current
	}
	if semver.Compare(b, a) > 0 {
		return next
	}
	return current
}

// goSemver normalizes a module version for golang.org/x/mod/semver.
func goSemver(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// goAttachSumEvidence merges every member's go.sum into the accumulators
// and returns modules that appear only in checksum evidence. Observed
// versions show what the module graph downloaded; they are not proof of
// the selected build-list version.
func goAttachSumEvidence(members []goMember, accs map[string]*goModuleAcc, memberPaths map[string]bool) []Module {
	var out []Module
	seen := map[string]bool{}
	for _, member := range members {
		for path, versions := range goSumVersions(filepath.Join(member.dir, "go.sum")) {
			if acc := accs[path]; acc != nil {
				acc.observed = append(acc.observed, versions...)
				continue
			}
			if memberPaths[path] || seen[path] {
				continue
			}
			seen[path] = true
			versions = dedupeSorted(versions)
			mod := Module{
				Name:             path,
				ObservedVersions: versions,
				Matchable:        true,
				Reachable:        true,
				SourceKind:       SourceGoModule,
				Confidence:       ConfidenceObserved,
			}
			if len(versions) == 1 {
				mod.Version = versions[0]
			}
			out = append(out, mod)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// goSumVersions parses go.sum into a module path to observed versions map.
// Both module-zip and go.mod checksum lines count as evidence that the
// version existed in the module graph.
func goSumVersions(path string) map[string][]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || strings.HasPrefix(line, "/") {
			continue
		}
		version := strings.TrimSuffix(fields[1], "/go.mod")
		out[fields[0]] = append(out[fields[0]], version)
	}
	return out
}

func dedupeSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	out := values[:1]
	for _, v := range values[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
