package security

import (
	"context"
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
// Prefer InventoryGoWithResolution, which establishes the selected build
// list and never reports ghost versions from stale go.sum entries.
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

// inventoryGoFallbackNotes is appended when the selected build list could
// not be established and manifest evidence is all a scan has.
func inventoryGoFallbackNotes(cause error) []string {
	return []string{
		"selected module versions could not be resolved (" + sanitizeOutcome(cause) + "); findings use manifest evidence and are conservative",
		"go.mod versions are build-list minimums; the selected version is not established",
		"go.sum supplies observed versions as evidence; it is not proof of the selected build-list version",
	}
}

// InventoryGoWithResolution inventories the Go dependencies of dir,
// resolving the selected build list through the given resolver. A
// resolution failure is not a scan failure: the inventory degrades to
// manifest evidence, with the limitation stated in the inventory notes.
// With resolution, every module carries the version the build actually
// selects, and stale go.sum checksum entries can no longer produce
// findings. The dependency graph is persisted when the module graph is
// available.
func InventoryGoWithResolution(ctx context.Context, dir string, resolver GoResolver) (*Inventory, error) {
	resolved, err := resolver.ResolveModules(ctx, dir)
	if err != nil {
		inv, invErr := InventoryGo(dir)
		if invErr != nil {
			return nil, invErr
		}
		inv.Notes = append(inventoryGoFallbackNotes(err), inv.Notes...)
		markUnresolvedGraph(inv, dir)
		return inv, nil
	}
	graphEdges, graphErr := resolver.ModuleGraph(ctx, dir)
	return inventoryGoFromResolution(dir, resolved, graphEdges, graphErr)
}

// markUnresolvedGraph states plainly that a fallback inventory carries no
// dependency graph.
func markUnresolvedGraph(inv *Inventory, dir string) {
	for i := range inv.Modules {
		inv.Modules[i].Key = ModuleKey(inv.Modules[i].Name, inv.Modules[i].Version)
	}
	inv.Roots = goRootsFromDisk(dir)
	inv.Notes = append(inv.Notes, "dependency edges require module resolution; the graph is not recorded in this inventory")
}

// goRootsFromDisk lists the scanned project's own module paths.
func goRootsFromDisk(dir string) []string {
	ws, err := loadGoWorkspace(dir)
	if err != nil {
		return nil
	}
	var roots []string
	for _, member := range ws.members {
		raw, err := os.ReadFile(filepath.Join(member.dir, "go.mod"))
		if err != nil {
			continue
		}
		mod, err := modfile.Parse(filepath.Join(member.rel, "go.mod"), raw, nil)
		if err != nil || mod.Module == nil {
			continue
		}
		roots = append(roots, mod.Module.Mod.Path)
	}
	return roots
}

// inventoryGoFromResolution builds the authoritative inventory: one entry
// per selected module, keyed by the effective (post-replace) identity.
// go.sum evidence is attached for diagnostics but never drives matching.
// When the module graph is available the requirement edges are projected
// onto the selected versions and persisted.
func inventoryGoFromResolution(dir string, resolved []ResolvedModule, graphEdges []ModuleGraphEdge, graphErr error) (*Inventory, error) {
	ws, err := loadGoWorkspace(dir)
	if err != nil {
		return nil, err
	}
	direct, declaredBy, err := goManifestEvidence(ws)
	if err != nil {
		return nil, err
	}
	observed := map[string][]string{}
	for _, member := range ws.members {
		for path, versions := range goSumVersions(filepath.Join(member.dir, "go.sum")) {
			observed[path] = append(observed[path], versions...)
		}
	}

	modules := make([]Module, 0, len(resolved))
	seen := map[string]bool{}
	for _, mod := range resolved {
		if mod.Path == "" {
			continue
		}
		if seen[mod.Path] {
			continue
		}
		seen[mod.Path] = true
		entry := Module{
			Name:       mod.Path,
			Key:        ModuleKey(mod.Path, mod.Version),
			Version:    mod.Version,
			Matchable:  true,
			Reachable:  true,
			SourceKind: SourceGoModule,
			Direct:     !mod.Indirect,
		}
		if who, ok := direct[mod.Path]; ok && who {
			entry.Direct = true
		}
		entry.DeclaredBy = declaredBy[mod.Path]
		if versions := dedupeSorted(observed[mod.ReplacedFrom]); len(versions) > 0 {
			entry.ObservedVersions = versions
		} else {
			entry.ObservedVersions = dedupeSorted(observed[mod.Path])
		}
		switch {
		case mod.LocalReplace:
			entry.Version = ""
			entry.ReplacedFrom = mod.ReplacedFrom
			entry.Resolved = false
			entry.Confidence = ConfidenceUnknown
			entry.Matchable = false
			entry.Notes = append(entry.Notes, "replaced by a local path; selected version not established")
		case mod.ReplacedFrom != "":
			entry.Version = mod.Version
			entry.ReplacedFrom = mod.ReplacedFrom
			entry.Resolved = true
			entry.ResolvedFrom = "replace directive"
			entry.Confidence = ConfidenceResolved
			entry.SourceKind = SourceReplacePinned
		default:
			entry.Resolved = true
			entry.ResolvedFrom = "go module graph"
			entry.Confidence = ConfidenceResolved
		}
		modules = append(modules, entry)
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Name < modules[j].Name })

	inv := &Inventory{
		Ecosystem: EcosystemGo,
		Level:     InventoryResolved,
		Modules:   modules,
		Roots:     goRootsFromDisk(dir),
		Notes: []string{
			"versions come from the Go module graph as the toolchain selects it (go list -m, proxy disabled)",
			"go.sum checksums are recorded as evidence only; historical entries cannot produce findings",
		},
	}
	if graphErr != nil {
		inv.Notes = append(inv.Notes, "dependency edges could not be resolved: "+sanitizeOutcome(graphErr))
		return inv, nil
	}
	inv.Edges = projectGoGraphEdges(inv.Roots, modules, graphEdges)
	return inv, nil
}

// projectGoGraphEdges maps raw requirement edges onto selected versions:
// a requirement edge names the version a parent requires, while the graph
// persists the version the build actually selects for the child.
func projectGoGraphEdges(roots []string, modules []Module, raw []ModuleGraphEdge) []GraphEdge {
	selected := map[string]string{}
	for _, mod := range modules {
		selected[mod.Name] = mod.Key
	}
	rootSet := map[string]bool{}
	for _, root := range roots {
		rootSet[root] = true
	}
	seen := map[GraphEdge]bool{}
	var edges []GraphEdge
	for _, edge := range raw {
		from, ok := selected[edge.ParentPath]
		switch {
		case ok:
		case rootSet[edge.ParentPath]:
			from = edge.ParentPath
		default:
			continue
		}
		to, ok := selected[edge.ChildPath]
		if !ok {
			continue
		}
		pair := GraphEdge{From: from, To: to}
		if seen[pair] {
			continue
		}
		seen[pair] = true
		edges = append(edges, pair)
	}
	return edges
}

// goManifestEvidence collects direct/indirect declarations and the
// declaring members per module path across the workspace, for annotation
// of the resolved build list.
func goManifestEvidence(ws *goWorkspace) (map[string]bool, map[string][]string, error) {
	direct := map[string]bool{}
	declaredBy := map[string][]string{}
	for _, member := range ws.members {
		raw, err := os.ReadFile(filepath.Join(member.dir, "go.mod"))
		if err != nil {
			return nil, nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", filepath.Join(member.rel, "go.mod"))
		}
		mod, err := modfile.Parse(filepath.Join(member.rel, "go.mod"), raw, nil)
		if err != nil {
			return nil, nil, phelixerr.Wrapf(phelixerr.CodeValidation, err, "%s is malformed", filepath.Join(member.rel, "go.mod"))
		}
		if mod.Module == nil || mod.Module.Mod.Path == "" {
			return nil, nil, phelixerr.Newf(phelixerr.CodeValidation, "%s has no module directive", filepath.Join(member.rel, "go.mod"))
		}
		for _, req := range mod.Require {
			if !req.Indirect {
				direct[req.Mod.Path] = true
			}
			declaredBy[req.Mod.Path] = append(declaredBy[req.Mod.Path], mod.Module.Mod.Path)
		}
	}
	return direct, declaredBy, nil
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
