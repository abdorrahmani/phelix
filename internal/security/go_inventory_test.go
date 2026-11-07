package security

import (
	"strings"
	"testing"
)

func findModule(t *testing.T, inv *Inventory, name string) Module {
	t.Helper()
	for _, mod := range inv.Modules {
		if mod.Name == name {
			return mod
		}
	}
	t.Fatalf("module %q not found in inventory with %d modules", name, len(inv.Modules))
	return Module{}
}

func findByReplacedFrom(t *testing.T, inv *Inventory, original string) Module {
	t.Helper()
	for _, mod := range inv.Modules {
		if mod.ReplacedFrom == original {
			return mod
		}
	}
	t.Fatalf("no module replaced from %q", original)
	return Module{}
}

func TestInventoryGo_Basic(t *testing.T) {
	inv, err := InventoryGo("testdata/gobasic")
	if err != nil {
		t.Fatalf("InventoryGo: %v", err)
	}
	if inv.Ecosystem != EcosystemGo || inv.Level != InventoryManifest {
		t.Fatalf("unexpected ecosystem/level: %s/%s", inv.Ecosystem, inv.Level)
	}

	fork := findModule(t, inv, "example.com/fork")
	if !fork.Resolved || fork.Version != "v1.5.0" || fork.ReplacedFrom != "example.com/direct" {
		t.Fatalf("fork resolved evidence wrong: %+v", fork)
	}
	if fork.Confidence != ConfidenceResolved || fork.SourceKind != SourceReplacePinned {
		t.Fatalf("fork confidence/source wrong: %+v", fork)
	}
	if !fork.Matchable || !fork.Direct {
		t.Fatalf("fork must be matchable and direct: %+v", fork)
	}
	if len(fork.ObservedVersions) != 0 {
		t.Fatalf("replaced module must not inherit the original path's go.sum evidence: %+v", fork.ObservedVersions)
	}

	pinned := findModule(t, inv, "example.com/pinned")
	if !pinned.Resolved || pinned.Version != "v1.0.1" || pinned.ReplacedFrom != "example.com/pinned" {
		t.Fatalf("pinned replacement wrong: %+v", pinned)
	}

	indirect := findModule(t, inv, "example.com/indirect")
	if indirect.Resolved || indirect.Version != "v0.1.0" || indirect.Direct {
		t.Fatalf("indirect declared minimum wrong: %+v", indirect)
	}
	if indirect.Confidence != ConfidenceDeclaredMinimum || !indirect.Matchable {
		t.Fatalf("indirect confidence wrong: %+v", indirect)
	}

	local := findByReplacedFrom(t, inv, "example.com/local")
	if local.Resolved || local.Matchable || local.Confidence != ConfidenceUnknown {
		t.Fatalf("local replacement must be unresolved and unmatchable: %+v", local)
	}

	observed := findModule(t, inv, "example.com/observed")
	if observed.Confidence != ConfidenceObserved || observed.Direct {
		t.Fatalf("observed-only module wrong: %+v", observed)
	}
	if got := strings.Join(observed.ObservedVersions, ","); got != "v1.0.0,v1.1.0" {
		t.Fatalf("observed versions wrong: %q", got)
	}
	if observed.Version != "" {
		t.Fatalf("two observed versions must not collapse into one: %q", observed.Version)
	}

	for _, mod := range inv.Modules {
		if mod.Name == "example.com/direct" {
			t.Fatalf("replaced original module %q must not appear", mod.Name)
		}
		if mod.Name == "example.com/m1" {
			t.Fatalf("member module must not appear as a dependency")
		}
	}

	joined := strings.Join(inv.Notes, "\n")
	if !strings.Contains(joined, "exclude") {
		t.Fatalf("exclude directives must be noted: %v", inv.Notes)
	}
}

func TestInventoryGo_Workspace(t *testing.T) {
	inv, err := InventoryGo("testdata/gowork")
	if err != nil {
		t.Fatalf("InventoryGo: %v", err)
	}

	shared := findModule(t, inv, "example.com/shared")
	if shared.Version != "v1.4.0" || !shared.Direct {
		t.Fatalf("workspace merge must keep the maximum declared minimum: %+v", shared)
	}
	if len(shared.DeclaredBy) != 2 {
		t.Fatalf("shared must be declared by both members: %+v", shared.DeclaredBy)
	}
	for _, mod := range inv.Modules {
		if mod.Name == "example.com/m1" || mod.Name == "example.com/m2" {
			t.Fatalf("member module %q must not appear as a dependency", mod.Name)
		}
	}
	joined := strings.Join(inv.Notes, "\n")
	if !strings.Contains(joined, "go.work") {
		t.Fatalf("workspace must be noted: %v", inv.Notes)
	}
}

func TestInventoryGo_Malformed(t *testing.T) {
	if _, err := InventoryGo("testdata/gomalformed"); err == nil {
		t.Fatal("malformed go.mod must fail")
	}
}
