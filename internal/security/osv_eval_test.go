package security

import (
	"context"
	"testing"
)

// multiSegment is the fix-then-regression shape: affected in [1.0, 2.0)
// and again in [5.0, 6.0), with a gap in between. Both segment orders are
// evaluated because the old folding implementation produced
// order-dependent verdicts.
func multiSegment(order int) []OSVRange {
	first := OSVRange{Type: "ECOSYSTEM", Events: events("introduced", "1.0.0", "fixed", "2.0.0")}
	second := OSVRange{Type: "ECOSYSTEM", Events: events("introduced", "5.0.0", "fixed", "6.0.0")}
	if order == 1 {
		return []OSVRange{second, first}
	}
	return []OSVRange{first, second}
}

func gapFinding(t *testing.T, confidence Confidence, version string, order int) Finding {
	t.Helper()
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/mod", Version: version, Confidence: confidence, Matchable: true},
	}}
	source := fakeSource{queryKey(EcosystemGo, "example.com/mod"): []AdvisoryEntry{
		{Advisory: mkAdv("GO-2099-0100", EcosystemGo, "example.com/mod", multiSegment(order), nil, "HIGH")},
	}}
	findings, err := MatchInventory(context.Background(), inv, source)
	if err != nil {
		t.Fatalf("MatchInventory: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("one finding expected: %+v", findings)
	}
	return findings[0]
}

// TestMatchInventory_MultiSegmentGap_DeclaredMinimumIsUnknown is the
// regression test for the false not_affected: a declared minimum in the
// gap of a fix-then-regression range leaves the selected version
// undetermined and must be unknown, regardless of segment order.
func TestMatchInventory_MultiSegmentGap_DeclaredMinimumIsUnknown(t *testing.T) {
	for _, order := range []int{0, 1} {
		finding := gapFinding(t, ConfidenceDeclaredMinimum, "v3.0.0", order)
		if finding.State != StateUnknown {
			t.Fatalf("order %d: a declared minimum in the gap must be unknown, got %s", order, finding.State)
		}
	}
}

// TestMatchInventory_MultiSegmentGap_ResolvedIsNotAffected pins the other
// side: a concrete resolved version in the gap is genuinely outside both
// segments.
func TestMatchInventory_MultiSegmentGap_ResolvedIsNotAffected(t *testing.T) {
	for _, order := range []int{0, 1} {
		finding := gapFinding(t, ConfidenceResolved, "v3.0.0", order)
		if finding.State != StateNotAffected {
			t.Fatalf("order %d: a resolved version in the gap is not affected, got %s", order, finding.State)
		}
	}
}

// TestMatchInventory_MultiSegment_InSecondSegment ensures membership in a
// later segment wins regardless of the order the advisory lists them.
func TestMatchInventory_MultiSegment_InSecondSegment(t *testing.T) {
	for _, order := range []int{0, 1} {
		resolved := gapFinding(t, ConfidenceResolved, "v5.5.0", order)
		if resolved.State != StateAffected {
			t.Fatalf("order %d: resolved version in the second segment is affected, got %s", order, resolved.State)
		}
		declared := gapFinding(t, ConfidenceDeclaredMinimum, "v5.5.0", order)
		if declared.State != StatePotentiallyAffected {
			t.Fatalf("order %d: declared minimum inside a bounded segment is potentially affected, got %s", order, declared.State)
		}
	}
}

// TestMatchInventory_MergeGroup_FixedVersionUsesEcosystemOrdering is the
// regression test for the lexicographic fixed-version maximum: 10.0.0
// must beat 9.0.0.
func TestMatchInventory_MergeGroup_FixedVersionUsesEcosystemOrdering(t *testing.T) {
	inv := &Inventory{Ecosystem: EcosystemGo, Modules: []Module{
		{Name: "example.com/mod", Version: "v1.0.0", Confidence: ConfidenceDeclaredMinimum, Matchable: true},
	}}
	first := mkAdv("GO-2099-0200", EcosystemGo, "example.com/mod",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "9.0.0")}}, nil, "HIGH")
	first.Aliases = []string{"CVE-2099-0200"}
	second := mkAdv("GHSA-2222-2222-2222", EcosystemGo, "example.com/mod",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "10.0.0")}}, nil, "HIGH")
	second.Aliases = []string{"CVE-2099-0200"}
	source := fakeSource{queryKey(EcosystemGo, "example.com/mod"): []AdvisoryEntry{
		{Advisory: first}, {Advisory: second},
	}}
	findings, err := MatchInventory(context.Background(), inv, source)
	if err != nil {
		t.Fatalf("MatchInventory: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("duplicate records must merge: %+v", findings)
	}
	if findings[0].FixedVersion != "10.0.0" {
		t.Fatalf("the highest fixed boundary must win: %s", findings[0].FixedVersion)
	}
}
