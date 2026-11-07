package security

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// fakeResolver is a scripted GoResolver.
type fakeResolver struct {
	modules  []ResolvedModule
	err      error
	graph    []ModuleGraphEdge
	graphErr error
}

func (f fakeResolver) ResolveModules(ctx context.Context, dir string) ([]ResolvedModule, error) {
	return f.modules, f.err
}

func (f fakeResolver) ModuleGraph(ctx context.Context, dir string) ([]ModuleGraphEdge, error) {
	return f.graph, f.graphErr
}

func writeGoFixture(t *testing.T, goMod, goSum string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if goSum != "" {
		if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte(goSum), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestInventoryGoWithResolution_NewerSelectedVersionRejectsGhost is the
// regression test for the manually reported case: go.sum carries an old
// pseudo-version of gopkg.in/yaml.v3 while the build selects v3.0.2. The
// advisory affecting everything below v3.0.1 must not fire, and no
// finding may display the stale pseudo-version as the dependency version.
func TestInventoryGoWithResolution_NewerSelectedVersionRejectsGhost(t *testing.T) {
	dir := writeGoFixture(t,
		"module example.com/app\n\ngo 1.24\n\nrequire gopkg.in/yaml.v3 v3.0.1 // indirect\n",
		"gopkg.in/yaml.v3 v3.0.0-20200313102051-9f266ea9e77c h1:AAA=\n"+
			"gopkg.in/yaml.v3 v3.0.1 h1:BBB=\n"+
			"gopkg.in/yaml.v3 v3.0.2 h1:CCC=\n")
	resolver := fakeResolver{modules: []ResolvedModule{
		{Path: "gopkg.in/yaml.v3", Version: "v3.0.2", Indirect: true},
	}}

	inv, err := InventoryGoWithResolution(context.Background(), dir, resolver)
	if err != nil {
		t.Fatalf("InventoryGoWithResolution: %v", err)
	}
	if inv.Level != InventoryResolved {
		t.Fatalf("resolution must yield a resolved inventory: %s", inv.Level)
	}
	mod := findModule(t, inv, "gopkg.in/yaml.v3")
	if mod.Version != "v3.0.2" || !mod.Resolved || mod.Confidence != ConfidenceResolved {
		t.Fatalf("the selected version must be authoritative: %+v", mod)
	}
	if mod.ResolvedFrom != "go module graph" {
		t.Fatalf("resolution provenance wrong: %s", mod.ResolvedFrom)
	}

	adv := mkAdv("GO-2022-0603", EcosystemGo, "gopkg.in/yaml.v3",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "3.0.1")}}, nil, "HIGH")
	findings, err := MatchInventory(context.Background(), inv, fakeSource{
		queryKey(EcosystemGo, "gopkg.in/yaml.v3"): []AdvisoryEntry{{Advisory: adv}},
	})
	if err != nil {
		t.Fatalf("MatchInventory: %v", err)
	}
	if len(findings) != 1 || findings[0].State != StateNotAffected {
		t.Fatalf("the selected v3.0.2 is outside the affected range: %+v", findings)
	}
	if findings[0].Version != "v3.0.2" {
		t.Fatalf("findings must show the selected version, never the go.sum ghost: %+v", findings[0])
	}
}

// TestInventoryGoWithResolution_SelectedVulnerableVersionIsAffected pins
// the other side: resolution must not suppress a real finding.
func TestInventoryGoWithResolution_SelectedVulnerableVersionIsAffected(t *testing.T) {
	dir := writeGoFixture(t,
		"module example.com/app\n\ngo 1.24\n\nrequire gopkg.in/yaml.v3 v3.0.2 // indirect\n", "")
	resolver := fakeResolver{modules: []ResolvedModule{
		{Path: "gopkg.in/yaml.v3", Version: "v3.0.2", Indirect: true},
	}}
	inv, err := InventoryGoWithResolution(context.Background(), dir, resolver)
	if err != nil {
		t.Fatalf("InventoryGoWithResolution: %v", err)
	}
	adv := mkAdv("GO-2099-0001", EcosystemGo, "gopkg.in/yaml.v3",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "3.0.0", "fixed", "3.0.3")}}, nil, "HIGH")
	findings, _ := MatchInventory(context.Background(), inv, fakeSource{
		queryKey(EcosystemGo, "gopkg.in/yaml.v3"): []AdvisoryEntry{{Advisory: adv}},
	})
	if len(findings) != 1 || findings[0].State != StateAffected {
		t.Fatalf("a resolved version inside the range is affected: %+v", findings)
	}
}

// TestInventoryGoWithResolution_FallbackKeepsConservativeFindings pins the
// degraded path: without resolution the declared minimum still produces a
// conservative potentially_affected finding, and the inventory says why.
func TestInventoryGoWithResolution_FallbackKeepsConservativeFindings(t *testing.T) {
	dir := writeGoFixture(t,
		"module example.com/app\n\ngo 1.24\n\nrequire example.com/mod v1.0.0\n",
		"example.com/mod v1.0.0 h1:AAA=\n")
	resolver := fakeResolver{err: errors.New("module lookup disabled")}

	inv, err := InventoryGoWithResolution(context.Background(), dir, resolver)
	if err != nil {
		t.Fatalf("fallback must not fail the scan: %v", err)
	}
	if inv.Level != InventoryManifest {
		t.Fatalf("fallback inventory is manifest-level: %s", inv.Level)
	}
	mod := findModule(t, inv, "example.com/mod")
	if mod.Resolved || mod.Confidence != ConfidenceDeclaredMinimum {
		t.Fatalf("fallback evidence is a declared minimum: %+v", mod)
	}
	joined := ""
	for _, note := range inv.Notes {
		joined += note + "\n"
	}
	if !strings.Contains(joined, "selected module versions could not be resolved") {
		t.Fatalf("resolution failure must be stated: %v", inv.Notes)
	}

	adv := mkAdv("GO-2099-0002", EcosystemGo, "example.com/mod",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "2.0.0")}}, nil, "LOW")
	findings, _ := MatchInventory(context.Background(), inv, fakeSource{
		queryKey(EcosystemGo, "example.com/mod"): []AdvisoryEntry{{Advisory: adv}},
	})
	if len(findings) != 1 || findings[0].State != StatePotentiallyAffected {
		t.Fatalf("fallback keeps the conservative finding: %+v", findings)
	}
}

// TestInventoryGoWithResolution_Replacements covers the two replacement
// shapes: pinned replacements are resolved identities, local replacements
// are explicitly unresolved and unmatchable.
func TestInventoryGoWithResolution_Replacements(t *testing.T) {
	dir := writeGoFixture(t,
		"module example.com/app\n\ngo 1.24\n\nrequire example.com/pinned v1.0.0\n", "")
	resolver := fakeResolver{modules: []ResolvedModule{
		{Path: "example.com/fork", Version: "v1.5.0", ReplacedFrom: "example.com/pinned"},
		{Path: "/tmp/local-lib", ReplacedFrom: "example.com/local", LocalReplace: true},
	}}
	inv, err := InventoryGoWithResolution(context.Background(), dir, resolver)
	if err != nil {
		t.Fatalf("InventoryGoWithResolution: %v", err)
	}
	pinned := findModule(t, inv, "example.com/fork")
	if !pinned.Resolved || pinned.Version != "v1.5.0" || pinned.ReplacedFrom != "example.com/pinned" {
		t.Fatalf("pinned replacement wrong: %+v", pinned)
	}
	if pinned.SourceKind != SourceReplacePinned {
		t.Fatalf("pinned replacement source wrong: %+v", pinned)
	}
	local := findModule(t, inv, "/tmp/local-lib")
	if local.Resolved || local.Matchable || local.Confidence != ConfidenceUnknown {
		t.Fatalf("local replacement must be unresolved and unmatchable: %+v", local)
	}
}

// TestToolchainGoResolver_RealToolchain exercises the production resolver
// against the real Go toolchain using this repository's own module graph,
// which is warm in the local module cache. Skipped when the toolchain or
// cache is unavailable so CI without Go stays green.
func TestToolchainGoResolver_RealToolchain(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Skip("repository go.mod unavailable")
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join("..", "..", "go.sum"))
	if err == nil {
		_ = os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644)
	}

	resolver := &ToolchainGoResolver{}
	modules, err := resolver.ResolveModules(context.Background(), dir)
	if err != nil {
		t.Skipf("toolchain resolution unavailable in this environment: %v", err)
	}
	if len(modules) == 0 {
		t.Fatal("this repository's module graph cannot be empty")
	}
	var modFile *modfile.File
	raw, err = os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err == nil {
		modFile, _ = modfile.Parse("go.mod", raw, nil)
	}
	if modFile != nil {
		for _, mod := range modules {
			if mod.Path == modFile.Module.Mod.Path {
				t.Fatalf("the main module must not appear in the resolved build list: %+v", mod)
			}
		}
	}
	for _, mod := range modules {
		if mod.Path == "golang.org/x/mod" && mod.Version == "" && !mod.LocalReplace {
			t.Fatalf("resolved modules carry selected versions: %+v", mod)
		}
	}
}

// TestInventoryGoWithResolution_GraphProjection pins the graph contract:
// requirement edges are projected onto the selected versions, the roots
// are the project's own modules, and duplicates collapse.
func TestInventoryGoWithResolution_GraphProjection(t *testing.T) {
	dir := writeGoFixture(t,
		"module example.com/app\n\ngo 1.24\n\nrequire example.com/a v1.0.0\n", "")
	resolver := fakeResolver{
		modules: []ResolvedModule{
			{Path: "example.com/a", Version: "v1.2.0", Indirect: false},
			{Path: "example.com/b", Version: "v2.0.1", Indirect: true},
		},
		graph: []ModuleGraphEdge{
			{ParentPath: "example.com/app", ChildPath: "example.com/a", ChildVersion: "v1.0.0"},
			{ParentPath: "example.com/app", ChildPath: "example.com/a", ChildVersion: "v1.0.0"},
			{ParentPath: "example.com/a", ParentVersion: "v1.0.0", ChildPath: "example.com/b", ChildVersion: "v2.0.0"},
		},
	}
	inv, err := InventoryGoWithResolution(context.Background(), dir, resolver)
	if err != nil {
		t.Fatalf("InventoryGoWithResolution: %v", err)
	}
	if len(inv.Roots) != 1 || inv.Roots[0] != "example.com/app" {
		t.Fatalf("the main module is the graph root: %v", inv.Roots)
	}
	want := map[GraphEdge]bool{
		{From: "example.com/app", To: "example.com/a@v1.2.0"}:      true,
		{From: "example.com/a@v1.2.0", To: "example.com/b@v2.0.1"}: true,
	}
	if len(inv.Edges) != len(want) {
		t.Fatalf("edges must be deduplicated and projected onto selected versions: %+v", inv.Edges)
	}
	for _, edge := range inv.Edges {
		if !want[edge] {
			t.Fatalf("unexpected edge %+v", edge)
		}
	}
}

// TestInventoryGoWithResolution_GraphUnavailable degrades gracefully: a
// module-graph failure keeps the resolved inventory and says the graph is
// missing.
func TestInventoryGoWithResolution_GraphUnavailable(t *testing.T) {
	dir := writeGoFixture(t,
		"module example.com/app\n\ngo 1.24\n\nrequire example.com/a v1.0.0\n", "")
	resolver := fakeResolver{
		modules:  []ResolvedModule{{Path: "example.com/a", Version: "v1.2.0", Indirect: false}},
		graphErr: errors.New("go mod graph failed"),
	}
	inv, err := InventoryGoWithResolution(context.Background(), dir, resolver)
	if err != nil {
		t.Fatalf("a graph failure must not fail the scan: %v", err)
	}
	if len(inv.Edges) != 0 {
		t.Fatalf("no edges without a graph: %+v", inv.Edges)
	}
	joined := ""
	for _, note := range inv.Notes {
		joined += note + "\n"
	}
	if !strings.Contains(joined, "dependency edges could not be resolved") {
		t.Fatalf("the limitation must be stated: %v", inv.Notes)
	}
}
