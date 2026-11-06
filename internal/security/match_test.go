package security

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeSource serves advisory entries from a fixed map keyed like queryKey.
type fakeSource map[string][]AdvisoryEntry

func (f fakeSource) Advisories(ctx context.Context, eco Ecosystem, name string) ([]AdvisoryEntry, error) {
	return f[queryKey(eco, name)], nil
}

func mkAdv(id string, eco Ecosystem, pkg string, ranges []OSVRange, versions []string, dbSeverity string) *OSVAdvisory {
	adv := &OSVAdvisory{ID: id, Summary: "advisory " + id}
	affected := OSVAffected{}
	affected.Package.Ecosystem, _ = osvEcosystemName(eco)
	affected.Package.Name = pkg
	affected.Ranges = ranges
	affected.Versions = versions
	if dbSeverity != "" {
		affected.DatabaseSpecific = map[string]any{"severity": dbSeverity}
	}
	adv.Affected = []OSVAffected{affected}
	return adv
}

func events(pairs ...string) []map[string]string {
	var out []map[string]string
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]string{pairs[i]: pairs[i+1]})
	}
	return out
}

func TestMatchInventory_GoDeclaredMinimum(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "golang.org/x/crypto", Version: "v0.10.0", Confidence: ConfidenceDeclaredMinimum, Matchable: true, Direct: true},
	}}
	source := fakeSource{queryKey(EcosystemGo, "golang.org/x/crypto"): []AdvisoryEntry{
		{Advisory: mkAdv("GO-2024-1001", EcosystemGo, "golang.org/x/crypto",
			[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "0.17.0")}}, nil, "HIGH"),
			FetchedAt: time.Now()},
	}}

	findings, err := MatchInventory(context.Background(), inv, source)
	if err != nil {
		t.Fatalf("MatchInventory: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("one finding expected: %+v", findings)
	}
	f := findings[0]
	if f.State != StatePotentiallyAffected {
		t.Fatalf("declared minimum inside a bounded range must be potentially affected: %+v", f)
	}
	if f.FixedVersion != "0.17.0" || f.Severity != SeverityHigh {
		t.Fatalf("severity and fixed version must flow through: %+v", f)
	}
	if f.Reachability != ReachabilityUnknown || !f.VersionResolved == false {
		t.Fatalf("declared minimum evidence is unresolved: %+v", f)
	}
}

func TestMatchInventory_GoDeclaredMinimumAboveFix(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "golang.org/x/crypto", Version: "v0.20.0", Confidence: ConfidenceDeclaredMinimum, Matchable: true},
	}}
	source := fakeSource{queryKey(EcosystemGo, "golang.org/x/crypto"): []AdvisoryEntry{
		{Advisory: mkAdv("GO-2024-1001", EcosystemGo, "golang.org/x/crypto",
			[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "0.17.0")}}, nil, "")},
	}}
	findings, _ := MatchInventory(context.Background(), inv, source)
	if len(findings) != 1 || findings[0].State != StateNotAffected {
		t.Fatalf("declared minimum above every fixed boundary must be not affected: %+v", findings)
	}
}

func TestMatchInventory_GoDeclaredMinimumBelowIntroduced(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/mod", Version: "v0.5.0", Confidence: ConfidenceDeclaredMinimum, Matchable: true},
	}}
	source := fakeSource{queryKey(EcosystemGo, "example.com/mod"): []AdvisoryEntry{
		{Advisory: mkAdv("GO-2024-1002", EcosystemGo, "example.com/mod",
			[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0.17.0", "fixed", "0.31.0")}}, nil, "")},
	}}
	findings, _ := MatchInventory(context.Background(), inv, source)
	if len(findings) != 1 || findings[0].State != StateUnknown {
		t.Fatalf("a minimum below the first affected version leaves the selection uncertain: %+v", findings)
	}
}

func TestMatchInventory_GoResolvedReplacement(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/fork", Version: "v1.5.0", ReplacedFrom: "example.com/mod",
			Confidence: ConfidenceResolved, Resolved: true, Matchable: true},
	}}
	source := fakeSource{queryKey(EcosystemGo, "example.com/fork"): []AdvisoryEntry{
		{Advisory: mkAdv("GO-2024-1003", EcosystemGo, "example.com/fork",
			[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "1.0.0", "fixed", "2.0.0")}}, nil, "CRITICAL")},
	}}
	findings, _ := MatchInventory(context.Background(), inv, source)
	if len(findings) != 1 || findings[0].State != StateAffected {
		t.Fatalf("a resolved replacement inside a range is affected: %+v", findings)
	}
	if !findings[0].VersionResolved || findings[0].Version != "v1.5.0" {
		t.Fatalf("resolved evidence must be marked resolved: %+v", findings[0])
	}
}

func TestMatchInventory_GoObservedEvidence(t *testing.T) {
	source := fakeSource{queryKey(EcosystemGo, "example.com/mod"): []AdvisoryEntry{
		{Advisory: mkAdv("GO-2024-1004", EcosystemGo, "example.com/mod",
			[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "1.5.0")}}, nil, "")},
	}}
	inInv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/mod", ObservedVersions: []string{"v1.4.0"}, Confidence: ConfidenceObserved, Matchable: true},
	}}
	findings, _ := MatchInventory(context.Background(), inInv, source)
	if len(findings) != 1 || findings[0].State != StatePotentiallyAffected {
		t.Fatalf("observed version inside the range is potentially affected: %+v", findings)
	}

	outInv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/mod", ObservedVersions: []string{"v1.6.0"}, Confidence: ConfidenceObserved, Matchable: true},
	}}
	findings, _ = MatchInventory(context.Background(), outInv, source)
	if len(findings) != 1 || findings[0].State != StateNotAffected {
		t.Fatalf("observed version past the fix is not affected: %+v", findings)
	}
}

func TestMatchInventory_GoPseudoVersionAndIncompatible(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/mod", Version: "v0.0.0-20230101000000-abcdef123456", Confidence: ConfidenceDeclaredMinimum, Matchable: true},
		{Name: "example.com/bigmod", Version: "v2.0.0+incompatible", Confidence: ConfidenceDeclaredMinimum, Matchable: true},
	}}
	source := fakeSource{
		queryKey(EcosystemGo, "example.com/mod"): []AdvisoryEntry{
			{Advisory: mkAdv("GO-2024-1005", EcosystemGo, "example.com/mod",
				[]OSVRange{{Type: "SEMVER", Events: events("introduced", "0", "fixed", "0.1.0")}}, nil, "")},
		},
		queryKey(EcosystemGo, "example.com/bigmod"): []AdvisoryEntry{
			{Advisory: mkAdv("GO-2024-1006", EcosystemGo, "example.com/bigmod",
				[]OSVRange{{Type: "SEMVER", Events: events("introduced", "1.0.0", "fixed", "1.5.0")}}, nil, "")},
		},
	}
	findings, err := MatchInventory(context.Background(), inv, source)
	if err != nil {
		t.Fatalf("MatchInventory: %v", err)
	}
	states := map[string]string{}
	for _, f := range findings {
		states[f.Package] = f.State
	}
	if states["example.com/mod"] != StatePotentiallyAffected {
		t.Fatalf("pseudo-version must compare as a pre-release of zero: %+v", states)
	}
	if states["example.com/bigmod"] != StateNotAffected {
		t.Fatalf("+incompatible versions must compare with plain semver: %+v", states)
	}
}

func TestMatchInventory_RustLockfile(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemRust, Modules: []Module{
		{Name: "crate", Version: "1.5.0", Confidence: ConfidenceResolved, Resolved: true, Matchable: true},
	}}
	source := fakeSource{queryKey(EcosystemRust, "crate"): []AdvisoryEntry{
		{Advisory: mkAdv("RUSTSEC-2024-0001", EcosystemRust, "crate",
			[]OSVRange{{Type: "SEMVER", Events: events("introduced", "1.0.0", "fixed", "1.9.0")}}, nil, "")},
	}}
	findings, _ := MatchInventory(context.Background(), inv, source)
	if len(findings) != 1 || findings[0].State != StateAffected {
		t.Fatalf("resolved lockfile version inside a range is affected: %+v", findings)
	}

	inv.Modules[0].Version = "2.0.0"
	findings, _ = MatchInventory(context.Background(), inv, source)
	if findings[0].State != StateNotAffected {
		t.Fatalf("resolved version past the fix is not affected: %+v", findings[0])
	}
}

func TestMatchInventory_RustLimitAndLastAffected(t *testing.T) {
	source := fakeSource{queryKey(EcosystemRust, "crate"): []AdvisoryEntry{
		{Advisory: mkAdv("RUSTSEC-2024-0002", EcosystemRust, "crate",
			[]OSVRange{{Type: "SEMVER", Events: events("introduced", "1.0.0", "limit", "2.0.0")}}, nil, "")},
		{Advisory: mkAdv("RUSTSEC-2024-0003", EcosystemRust, "crate",
			[]OSVRange{{Type: "SEMVER", Events: events("introduced", "1.0.0", "last_affected", "1.5.0")}}, nil, "")},
	}}

	inside := &Inventory{Ecosystem: EcosystemRust, Modules: []Module{
		{Name: "crate", Version: "1.5.0", Confidence: ConfidenceResolved, Resolved: true, Matchable: true},
	}}
	findings, _ := MatchInventory(context.Background(), inside, source)
	states := map[string]string{}
	for _, f := range findings {
		states[f.AdvisoryID] = f.State
	}
	if states["RUSTSEC-2024-0002"] != StateAffected || states["RUSTSEC-2024-0003"] != StateAffected {
		t.Fatalf("versions below limit and at last_affected are inside: %+v", states)
	}

	beyond := &Inventory{Ecosystem: EcosystemRust, Modules: []Module{
		{Name: "crate", Version: "2.5.0", Confidence: ConfidenceResolved, Resolved: true, Matchable: true},
	}}
	findings, _ = MatchInventory(context.Background(), beyond, source)
	states = map[string]string{}
	for _, f := range findings {
		states[f.AdvisoryID] = f.State
	}
	if states["RUSTSEC-2024-0002"] != StateUnknown {
		t.Fatalf("a version beyond a limit boundary must stay unknown: %+v", states)
	}
	if states["RUSTSEC-2024-0003"] != StateNotAffected {
		t.Fatalf("a version past last_affected is not affected: %+v", states)
	}
}

func TestMatchInventory_RustManifestRequirement(t *testing.T) {
	source := fakeSource{queryKey(EcosystemRust, "crate"): []AdvisoryEntry{
		{Advisory: mkAdv("RUSTSEC-2024-0004", EcosystemRust, "crate",
			[]OSVRange{{Type: "SEMVER", Events: events("introduced", "1.5.0", "fixed", "1.9.0")}}, nil, "")},
	}}
	overlap := &Inventory{Ecosystem: EcosystemRust, Level: InventoryManifest, Modules: []Module{
		{Name: "crate", Requirement: "1.0", Confidence: ConfidenceRequirement, Matchable: true},
	}}
	findings, _ := MatchInventory(context.Background(), overlap, source)
	if len(findings) != 1 || findings[0].State != StatePotentiallyAffected {
		t.Fatalf("overlapping requirement is potentially affected: %+v", findings)
	}

	disjoint := &Inventory{Ecosystem: EcosystemRust, Level: InventoryManifest, Modules: []Module{
		{Name: "crate", Requirement: "0.5", Confidence: ConfidenceRequirement, Matchable: true},
	}}
	findings, _ = MatchInventory(context.Background(), disjoint, source)
	if len(findings) != 1 || findings[0].State != StateNotAffected {
		t.Fatalf("disjoint requirement cannot admit an affected version: %+v", findings)
	}
}

func TestMatchInventory_GitRangeUnevaluable(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemRust, Modules: []Module{
		{Name: "crate", Version: "1.0.0", Confidence: ConfidenceResolved, Resolved: true, Matchable: true},
	}}
	source := fakeSource{queryKey(EcosystemRust, "crate"): []AdvisoryEntry{
		{Advisory: mkAdv("RUSTSEC-2024-0005", EcosystemRust, "crate",
			[]OSVRange{{Type: "GIT", Repo: "https://example.com/repo", Events: events("introduced", "0")}}, nil, "")},
	}}
	findings, _ := MatchInventory(context.Background(), inv, source)
	if len(findings) != 1 || findings[0].State != StateUnknown {
		t.Fatalf("git ranges must produce explicit unknown findings: %+v", findings)
	}
	if !strings.Contains(findings[0].Reason, "cannot be evaluated") {
		t.Fatalf("reason must explain the limitation: %s", findings[0].Reason)
	}
}

func TestMatchInventory_AliasDedup(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/mod", Version: "v1.0.0", Confidence: ConfidenceDeclaredMinimum, Matchable: true},
	}}
	first := mkAdv("GO-2024-0007", EcosystemGo, "example.com/mod",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "2.0.0")}}, nil, "HIGH")
	first.Aliases = []string{"CVE-2024-0007", "GHSA-7777-7777-7777"}
	second := mkAdv("GHSA-7777-7777-7777", EcosystemGo, "example.com/mod",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "2.0.0")}}, nil, "HIGH")
	source := fakeSource{queryKey(EcosystemGo, "example.com/mod"): []AdvisoryEntry{
		{Advisory: first}, {Advisory: second},
	}}
	findings, _ := MatchInventory(context.Background(), inv, source)
	if len(findings) != 1 {
		t.Fatalf("records sharing an alias must merge into one finding: %+v", findings)
	}
	if findings[0].AdvisoryID != "GO-2024-0007" {
		t.Fatalf("the ecosystem-native record must be primary: %+v", findings[0])
	}
	found := false
	for _, alias := range findings[0].Aliases {
		if alias == "GHSA-7777-7777-7777" {
			found = true
		}
	}
	if !found {
		t.Fatalf("merged aliases must keep the duplicate record's id: %+v", findings[0].Aliases)
	}
}

func TestMatchInventory_SeverityUnknownStaysUnknown(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/mod", Version: "v1.0.0", Confidence: ConfidenceDeclaredMinimum, Matchable: true},
	}}
	adv := mkAdv("GO-2024-1008", EcosystemGo, "example.com/mod",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0")}}, nil, "")
	adv.Severity = []OSVSeverity{{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}}
	source := fakeSource{queryKey(EcosystemGo, "example.com/mod"): []AdvisoryEntry{{Advisory: adv}}}
	findings, _ := MatchInventory(context.Background(), inv, source)
	if findings[0].Severity != SeverityUnknown {
		t.Fatalf("absent labels must stay unknown, not become medium: %+v", findings[0])
	}
	if len(findings[0].CVSSVectors) != 1 || !strings.HasPrefix(findings[0].CVSSVectors[0], "CVSS:3.1/") {
		t.Fatalf("raw CVSS vectors must be preserved: %+v", findings[0].CVSSVectors)
	}
}

func TestMatchInventory_UnmatchableModulesSkipped(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemRust, Modules: []Module{
		{Name: "gitdep", Version: "0.5.0", Confidence: ConfidenceResolved, SourceKind: SourceGit, Matchable: false},
	}}
	var calls int
	source := countingSource{inner: fakeSource{}, count: &calls}
	findings, err := MatchInventory(context.Background(), inv, source)
	if err != nil {
		t.Fatalf("MatchInventory: %v", err)
	}
	if len(findings) != 0 || calls != 0 {
		t.Fatalf("unmatchable modules must produce no queries or findings: findings=%d calls=%d", len(findings), calls)
	}
}

type countingSource struct {
	inner AdvisorySource
	count *int
}

func (c countingSource) Advisories(ctx context.Context, eco Ecosystem, name string) ([]AdvisoryEntry, error) {
	*c.count++
	return c.inner.Advisories(ctx, eco, name)
}

func TestMatchInventory_SharesQueriesAcrossLockfileVersions(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemRust, Modules: []Module{
		{Name: "twovers", Version: "1.0.5", Confidence: ConfidenceResolved, Matchable: true},
		{Name: "twovers", Version: "2.0.0", Confidence: ConfidenceResolved, Matchable: true},
	}}
	var calls int
	source := countingSource{
		inner: fakeSource{queryKey(EcosystemRust, "twovers"): []AdvisoryEntry{
			{Advisory: mkAdv("RUSTSEC-2024-0006", EcosystemRust, "twovers",
				[]OSVRange{{Type: "SEMVER", Events: events("introduced", "2.0.0")}}, nil, "")},
		}},
		count: &calls,
	}
	findings, err := MatchInventory(context.Background(), inv, source)
	if err != nil {
		t.Fatalf("MatchInventory: %v", err)
	}
	if calls != 1 {
		t.Fatalf("one package identity must produce one query: %d", calls)
	}
	affected := 0
	for _, f := range findings {
		if f.State == StateAffected && f.Version == "2.0.0" {
			affected++
		}
	}
	if affected != 1 {
		t.Fatalf("only the lockfile entry inside the range is affected: %+v", findings)
	}
}

func TestSeverityRank(t *testing.T) {
	order := []string{SeverityUnknown, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}
	for i := 1; i < len(order); i++ {
		if SeverityRank(order[i-1]) >= SeverityRank(order[i]) {
			t.Fatalf("%s must rank below %s", order[i-1], order[i])
		}
	}
	if SeverityRank("bogus") != SeverityRank(SeverityUnknown) {
		t.Fatal("unrecognized labels rank as unknown")
	}
}
