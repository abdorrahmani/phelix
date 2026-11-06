package security

import (
	"strings"
	"testing"
)

func TestInventoryRust_WorkspaceLock(t *testing.T) {
	inv, err := InventoryRust("testdata/rustws")
	if err != nil {
		t.Fatalf("InventoryRust: %v", err)
	}
	if inv.Ecosystem != EcosystemRust || inv.Level != InventoryResolved {
		t.Fatalf("unexpected ecosystem/level: %s/%s", inv.Ecosystem, inv.Level)
	}

	serde := findModule(t, inv, "serde")
	if serde.Version != "1.0.200" || !serde.Resolved || serde.ResolvedFrom != "cargo.lock" {
		t.Fatalf("serde resolution wrong: %+v", serde)
	}
	if !serde.Direct || !serde.Matchable || serde.Dev {
		t.Fatalf("serde direct classification wrong: %+v", serde)
	}
	if len(serde.DeclaredBy) != 2 {
		t.Fatalf("serde declared by both members: %+v", serde.DeclaredBy)
	}

	renamed := findModule(t, inv, "actual-crate")
	if !renamed.Direct || len(renamed.DeclaredBy) != 1 || renamed.DeclaredBy[0] != "alpha" {
		t.Fatalf("renamed dependency must resolve to the real crate name: %+v", renamed)
	}

	opt := findModule(t, inv, "optdep")
	if !opt.Optional {
		t.Fatalf("optional declaration lost: %+v", opt)
	}

	dev := findModule(t, inv, "testkit")
	if !dev.Dev || !dev.Direct {
		t.Fatalf("dev dependency classification wrong: %+v", dev)
	}

	git := findModule(t, inv, "gitdep")
	if git.SourceKind != SourceGit || git.Matchable {
		t.Fatalf("git dependency must be unmatchable: %+v", git)
	}
	if !git.Resolved {
		t.Fatalf("git dependency still has a resolved lockfile version: %+v", git)
	}

	path := findModule(t, inv, "localdep")
	if path.SourceKind != SourcePath || path.Matchable {
		t.Fatalf("path dependency must be unmatchable: %+v", path)
	}

	inherited := findModule(t, inv, "shared")
	if !inherited.Direct || inherited.Version != "1.0.4" {
		t.Fatalf("workspace-inherited dependency wrong: %+v", inherited)
	}

	unix := findModule(t, inv, "unixonly")
	if !unix.Direct {
		t.Fatalf("target-specific dependency counts as direct: %+v", unix)
	}

	var twovers []Module
	for _, mod := range inv.Modules {
		if mod.Name == "twovers" {
			twovers = append(twovers, mod)
		}
	}
	if len(twovers) != 2 {
		t.Fatalf("both versions of twovers must be present: %d", len(twovers))
	}
	directCount := 0
	for _, mod := range twovers {
		if mod.Direct {
			directCount++
		}
	}
	if directCount != 1 {
		t.Fatalf("requirement 1.0 must mark exactly one lockfile entry direct: %+v", twovers)
	}

	orphan := Module{}
	for _, mod := range inv.Modules {
		if mod.Name == "orphan" {
			orphan = mod
		}
	}
	if orphan.Name == "" {
		t.Fatal("orphan missing")
	}
	if orphan.Reachable {
		t.Fatalf("orphan must be unreachable: %+v", orphan)
	}
	if !strings.Contains(strings.Join(orphan.Notes, "\n"), "not reachable") {
		t.Fatalf("orphan note missing: %+v", orphan.Notes)
	}

	for _, mod := range inv.Modules {
		if mod.Name == "alpha" || mod.Name == "beta" {
			t.Fatalf("member crate %q must not appear as a dependency", mod.Name)
		}
	}
}

func TestInventoryRust_NoLock(t *testing.T) {
	inv, err := InventoryRust("testdata/rustnolock")
	if err != nil {
		t.Fatalf("InventoryRust: %v", err)
	}
	if inv.Level != InventoryManifest {
		t.Fatalf("missing lockfile must degrade to manifest level: %s", inv.Level)
	}

	serde := findModule(t, inv, "serde")
	if serde.Resolved || serde.Version != "" {
		t.Fatalf("manifest requirement must never look like a resolved version: %+v", serde)
	}
	if serde.Requirement != "1.0" || serde.Confidence != ConfidenceRequirement || !serde.Matchable {
		t.Fatalf("serde requirement evidence wrong: %+v", serde)
	}

	git := findModule(t, inv, "gitdep")
	if git.Matchable || git.SourceKind != SourceGit {
		t.Fatalf("git dependency must be unmatchable without a lockfile: %+v", git)
	}

	joined := strings.Join(inv.Notes, "\n")
	if !strings.Contains(joined, "Cargo.lock is missing") {
		t.Fatalf("missing lockfile must be noted: %v", inv.Notes)
	}
}

func TestInventoryRust_MalformedLock(t *testing.T) {
	if _, err := InventoryRust("testdata/rustmalformed"); err == nil {
		t.Fatal("malformed Cargo.lock must fail closed")
	}
}
