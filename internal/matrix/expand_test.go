package matrix

import (
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
)

func mustExpand(t *testing.T, versions, platforms []string, rules RuleSet) *MatrixPlan {
	t.Helper()
	plan, err := Expand(builder.Go, versions, platforms, rules)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func rule(fields map[string]string) Rule {
	return RuleFromFields(fields)
}

// --- Backward compatibility -------------------------------------------------

func TestExpand_NoRulesMatchesLegacyBehavior(t *testing.T) {
	plan := mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"}, RuleSet{})
	legacy, err := ParsePlan(builder.Go, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Combinations) != 4 || len(legacy.Combinations) != 4 {
		t.Fatalf("counts: expand=%d legacy=%d", len(plan.Combinations), len(legacy.Combinations))
	}
	for i := range plan.Combinations {
		if plan.Combinations[i].ID() != legacy.Combinations[i].ID() {
			t.Fatalf("order diverged at %d: %s vs %s", i, plan.Combinations[i].ID(), legacy.Combinations[i].ID())
		}
	}
	if plan.BaseCount != 4 || plan.IncludedCount != 0 || plan.ExcludedCount != 0 {
		t.Fatalf("counts: %+v", plan)
	}
}

func TestExpand_DeterministicOrdering(t *testing.T) {
	rules := RuleSet{
		Include: []Rule{rule(map[string]string{"go": "1.28", "platform": "linux/amd64"})},
		Exclude: []Rule{rule(map[string]string{"go": "1.26", "platform": "linux/arm64"})},
	}
	first := comboIDs(mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"}, rules))
	for i := 0; i < 20; i++ {
		again := comboIDs(mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"}, rules))
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("expansion order not deterministic (run %d, index %d):\n%v\n%v", i, j, first, again)
			}
		}
	}
}

// --- Exclude ----------------------------------------------------------------

func TestExpand_ExcludeOneCombination(t *testing.T) {
	// 3 versions × 4 platforms = 12; exclude 1 → 11.
	plan := mustExpand(t,
		[]string{"1.25", "1.26", "1.27"},
		[]string{"linux/amd64", "linux/arm64", "windows/amd64", "darwin/arm64"},
		RuleSet{Exclude: []Rule{rule(map[string]string{"go": "1.25", "platform": "windows/amd64"})}},
	)
	if len(plan.Combinations) != 11 {
		t.Fatalf("combinations = %d, want 11", len(plan.Combinations))
	}
	if plan.BaseCount != 12 || plan.ExcludedCount != 1 {
		t.Fatalf("counts: %+v", plan)
	}
	for _, id := range comboIDs(plan) {
		if id == "go1.25-windows-amd64" {
			t.Fatal("excluded combination still present")
		}
	}
}

func TestExpand_ExcludeMultipleRules(t *testing.T) {
	plan := mustExpand(t,
		[]string{"1.25", "1.26", "1.27"},
		[]string{"linux/amd64", "linux/arm64", "windows/amd64", "darwin/arm64"},
		RuleSet{Exclude: []Rule{
			rule(map[string]string{"go": "1.25", "platform": "windows/amd64"}),
			rule(map[string]string{"go": "1.26", "platform": "darwin/arm64"}),
		}},
	)
	if len(plan.Combinations) != 10 {
		t.Fatalf("combinations = %d, want 10", len(plan.Combinations))
	}
}

func TestExpand_ExcludePartialDimensionMatchesAll(t *testing.T) {
	// Excluding a whole version removes every combination of that version.
	plan := mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"},
		RuleSet{Exclude: []Rule{rule(map[string]string{"go": "1.26"})}})
	if len(plan.Combinations) != 2 {
		t.Fatalf("combinations = %d, want 2", len(plan.Combinations))
	}
	for _, id := range comboIDs(plan) {
		if len(id) >= 6 && id[:6] == "go1.26" {
			t.Fatalf("version-dimension exclude left %s behind", id)
		}
	}
}

func TestExpand_ExcludeByArchOnly(t *testing.T) {
	plan := mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"},
		RuleSet{Exclude: []Rule{rule(map[string]string{"arch": "arm64"})}})
	if len(plan.Combinations) != 2 {
		t.Fatalf("combinations = %d, want 2 (all arm64 removed)", len(plan.Combinations))
	}
}

func TestExpand_ExcludeMatchingNothingIsError(t *testing.T) {
	_, err := Expand(builder.Go, []string{"1.26"}, []string{"linux/amd64"},
		RuleSet{Exclude: []Rule{rule(map[string]string{"go": "9.99"})}})
	if err == nil {
		t.Fatal("no-match exclude accepted")
	}
}

func TestExpand_ExcludeInvalidRuleIsError(t *testing.T) {
	_, err := Expand(builder.Go, []string{"1.26"}, []string{"linux/amd64"},
		RuleSet{Exclude: []Rule{rule(map[string]string{"go": ""})}})
	if err == nil {
		t.Fatal("empty-value exclude accepted")
	}
	_, err = Expand(builder.Go, []string{"1.26"}, []string{"linux/amd64"},
		RuleSet{Exclude: []Rule{rule(map[string]string{"cgo": "false"})}})
	if err == nil {
		t.Fatal("unknown-dimension exclude accepted")
	}
}

// --- Include ----------------------------------------------------------------

func TestExpand_IncludeAddsCombinationOutsideProduct(t *testing.T) {
	plan := mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"},
		RuleSet{Include: []Rule{rule(map[string]string{"go": "1.28", "platform": "linux/amd64"})}})
	if len(plan.Combinations) != 5 {
		t.Fatalf("combinations = %d, want 5", len(plan.Combinations))
	}
	if plan.BaseCount != 4 || plan.IncludedCount != 1 {
		t.Fatalf("counts: %+v", plan)
	}
	found := false
	for _, c := range plan.Combinations {
		if c.ID() == "go1.28-linux-amd64" {
			found = true
		}
	}
	if !found {
		t.Fatalf("included combination missing: %v", comboIDs(plan))
	}
}

func TestExpand_IncludeDuplicateDoesNotDuplicateExecution(t *testing.T) {
	plan := mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64"},
		RuleSet{Include: []Rule{
			rule(map[string]string{"go": "1.27", "platform": "linux/amd64"}),
			rule(map[string]string{"go": "1.27", "platform": "linux/amd64", "tag": "a"}),
		}})
	if len(plan.Combinations) != 2 {
		t.Fatalf("combinations = %d, want 2 (duplicate include must not duplicate the job)", len(plan.Combinations))
	}
	if plan.IncludedCount != 0 {
		t.Fatalf("duplicate include counted as new: %+v", plan)
	}
	// The second rule's metadata must still be merged.
	for _, c := range plan.Combinations {
		if c.ID() == "go1.27-linux-amd64" && c.Metadata["tag"] != "a" {
			t.Fatalf("metadata not merged into existing combination: %+v", c.Metadata)
		}
	}
}

func TestExpand_IncludeMetadataMergedIntoMatching(t *testing.T) {
	// Full-rule include that matches an existing combination merges metadata.
	plan := mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64"},
		RuleSet{Include: []Rule{rule(map[string]string{"go": "1.27", "platform": "linux/amd64", "tag": "latest"})}})
	if len(plan.Combinations) != 2 {
		t.Fatalf("combinations = %d, want 2", len(plan.Combinations))
	}
	for _, c := range plan.Combinations {
		if c.ID() == "go1.27-linux-amd64" && c.Metadata["tag"] != "latest" {
			t.Fatalf("metadata not merged: %+v", c.Metadata)
		}
	}
}

func TestExpand_IncludePartialRuleMergesMetadata(t *testing.T) {
	// Partial include (no platform) attaches metadata to every matching
	// combination.
	plan := mustExpand(t, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"},
		RuleSet{Include: []Rule{rule(map[string]string{"go": "1.27", "tag": "edge"})}})
	for _, c := range plan.Combinations {
		if c.Version == "1.27" && c.Metadata["tag"] != "edge" {
			t.Fatalf("partial include metadata missing on %s: %+v", c.ID(), c.Metadata)
		}
		if c.Version == "1.26" && c.Metadata != nil {
			t.Fatalf("partial include leaked to non-matching %s", c.ID())
		}
	}
}

func TestExpand_IncludePartialRuleMatchingNothingIsError(t *testing.T) {
	_, err := Expand(builder.Go, []string{"1.26"}, []string{"linux/amd64"},
		RuleSet{Include: []Rule{rule(map[string]string{"go": "9.99", "tag": "x"})}})
	if err == nil {
		t.Fatal("no-match partial include accepted")
	}
}

func TestExpand_IncludeThenExcludeOrdering(t *testing.T) {
	// Documented pipeline: include runs before exclude, so an exclude can
	// remove an included combination.
	plan := mustExpand(t, []string{"1.26"}, []string{"linux/amd64"},
		RuleSet{
			Include: []Rule{rule(map[string]string{"go": "1.28", "platform": "linux/amd64"})},
			Exclude: []Rule{rule(map[string]string{"go": "1.28"})},
		})
	if len(plan.Combinations) != 1 {
		t.Fatalf("combinations = %d, want 1 (the included one is excluded again)", len(plan.Combinations))
	}
	if comboIDs(plan)[0] != "go1.26-linux-amd64" {
		t.Fatalf("unexpected survivor: %v", comboIDs(plan))
	}
	if plan.BaseCount != 1 || plan.IncludedCount != 1 || plan.ExcludedCount != 1 {
		t.Fatalf("counts: %+v", plan)
	}
}

func TestExpand_ExcludeCannotRemoveWhatEarlierExcludeTook(t *testing.T) {
	// Overlapping excludes: the second rule matches nothing because the
	// first already removed its targets — an error, not a silent no-op.
	_, err := Expand(builder.Go, []string{"1.26"}, []string{"linux/amd64"},
		RuleSet{Exclude: []Rule{
			rule(map[string]string{"go": "1.26"}),
			rule(map[string]string{"go": "1.26", "platform": "linux/amd64"}),
		}})
	if err == nil {
		t.Fatal("superseded exclude accepted silently")
	}
}

func TestExpand_IncludeInvalidCombinationIsError(t *testing.T) {
	_, err := Expand(builder.Go, []string{"1.26"}, []string{"linux/amd64"},
		RuleSet{Include: []Rule{rule(map[string]string{"go": "not-a-version", "platform": "linux/amd64"})}})
	if err == nil {
		t.Fatal("invalid include combination accepted")
	}
}

// --- Profile plumbing ---------------------------------------------------------

func TestProfilePlanAppliesRules(t *testing.T) {
	prof := &Profile{
		Lang:      builder.Go,
		Versions:  []string{"1.26", "1.27"},
		Platforms: []string{"linux/amd64", "linux/arm64"},
		Exclude:   []Rule{rule(map[string]string{"go": "1.26", "platform": "linux/arm64"})},
	}
	plan, err := prof.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Combinations) != 3 {
		t.Fatalf("combinations = %d, want 3", len(plan.Combinations))
	}
	if plan.ExcludedCount != 1 {
		t.Fatalf("counts: %+v", plan)
	}
}
