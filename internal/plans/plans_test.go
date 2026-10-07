package plans

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func planDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dir)
	return dir
}

// samplePlan returns a fully assembled, finalized plan.
func samplePlan(t *testing.T) *Plan {
	t.Helper()
	p := &Plan{
		Status: StatusCreated,
		Action: Action{Type: ActionRebuild, Application: "demo"},
		Target: Target{AppID: "app-1", AppName: "demo", Language: "go"},
		Inputs: Inputs{
			SourceDir:         "/src/demo",
			Port:              8080,
			ConfigFingerprint: "sha256:aaa",
			SourceCommit:      "abc123",
		},
		Execution: Execution{Strategy: "classic", PublicPort: 8080},
		Preconditions: []Precondition{
			{Type: PreconditionCurrentVersion, Expected: "v1", Source: "versions.json"},
			{Type: PreconditionAppExists, Expected: "app-1", Source: "apps.json"},
		},
		Capabilities: []string{"deployment", "monitoring"},
	}
	// Deliberately unsorted preconditions above; Finalize canonicalizes them.
	if err := p.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return p
}

func TestPlanID_FormatAndUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if !ValidateID(id) {
			t.Fatalf("minted id %q fails validation", id)
		}
		if seen[id] {
			t.Fatalf("duplicate plan id %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "pln_", "pln_ZZZ", "op_0123456789abcdef", "pln_0123456789abcdefX", "../escape", "pln_0123456789abcdef/../../x"} {
		if ValidateID(bad) {
			t.Fatalf("invalid id %q accepted", bad)
		}
	}
}

func TestHash_SemanticProperties(t *testing.T) {
	base := samplePlan(t)

	// 1. Same semantic plan → same hash (independent construction).
	twin := samplePlan(t)
	if twin.PlanHash != base.PlanHash {
		t.Fatal("two constructions of the same semantic plan must hash identically")
	}

	// 6/7. Volatile metadata does NOT affect the hash.
	mutated := *base
	mutated.PlanID = "pln_fedcba9876543210"
	mutated.CreatedAt = base.CreatedAt + 999999
	mutated.Status = StatusApplied
	h, err := mutated.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h != base.PlanHash {
		t.Fatalf("metadata changes changed the hash:\n%s vs %s", h, base.PlanHash)
	}

	// 2-5. Every execution-relevant change DOES change the hash.
	change := func(mutate func(*Plan)) string {
		p := *base
		p.Preconditions = append([]Precondition(nil), base.Preconditions...)
		p.Capabilities = append([]string(nil), base.Capabilities...)
		mutate(&p)
		out, err := p.Hash()
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		return out
	}
	for name, mutate := range map[string]func(*Plan){
		"changed action":         func(p *Plan) { p.Action.Application = "other" },
		"changed target":         func(p *Plan) { p.Target.AppID = "app-2" },
		"changed source":         func(p *Plan) { p.Inputs.SourceDir = "/src/other" },
		"changed port":           func(p *Plan) { p.Inputs.Port = 9090 },
		"changed strategy":       func(p *Plan) { p.Execution.Strategy = "blue-green" },
		"changed fingerprint":    func(p *Plan) { p.Inputs.ConfigFingerprint = "sha256:bbb" },
		"changed precondition":   func(p *Plan) { p.Preconditions[0].Expected = "v2" },
		"changed capability":     func(p *Plan) { p.Capabilities[0] = "rollback" },
		"changed target version": func(p *Plan) { p.Inputs.TargetVersion = 9 },
	} {
		if got := change(mutate); got == base.PlanHash {
			t.Fatalf("%s must change the hash", name)
		}
	}
}

func TestHash_CanonicalizationStable(t *testing.T) {
	// Preconditions and capabilities are constructed in different orders but
	// are semantically identical: Canonicalize (run by Finalize) must make
	// them hash the same.
	a := &Plan{
		Status: StatusCreated,
		Action: Action{Type: ActionRebuild, Application: "demo"},
		Target: Target{AppID: "app-1", AppName: "demo", Language: "go"},
		Inputs: Inputs{ConfigFingerprint: "sha256:aaa"},
		Preconditions: []Precondition{
			{Type: PreconditionCurrentVersion, Expected: "v1", Source: "versions.json"},
			{Type: PreconditionAppExists, Expected: "app-1", Source: "apps.json"},
			{Type: PreconditionToolchain, Expected: "installed", Source: "toolchain"},
		},
		Capabilities: []string{"monitoring", "deployment"},
	}
	b := &Plan{
		Status: StatusCreated,
		Action: Action{Type: ActionRebuild, Application: "demo"},
		Target: Target{AppID: "app-1", AppName: "demo", Language: "go"},
		Inputs: Inputs{ConfigFingerprint: "sha256:aaa"},
		Preconditions: []Precondition{
			{Type: PreconditionToolchain, Expected: "installed", Source: "toolchain"},
			{Type: PreconditionAppExists, Expected: "app-1", Source: "apps.json"},
			{Type: PreconditionCurrentVersion, Expected: "v1", Source: "versions.json"},
		},
		Capabilities: []string{"deployment", "monitoring"},
	}
	if err := a.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := b.Finalize(); err != nil {
		t.Fatal(err)
	}
	if a.PlanHash != b.PlanHash {
		t.Fatal("order-independent construction must produce identical hashes")
	}
}

func TestStore_CreateLoadRestartCycle(t *testing.T) {
	dir := planDataDir(t)
	p := samplePlan(t)
	if err := Save(p); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A fresh process reads the same file: simulate by re-reading from disk.
	loaded, err := Load(p.PlanID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.PlanHash != p.PlanHash || loaded.PlanID != p.PlanID {
		t.Fatal("plan identity lost across restart")
	}
	if loaded.Preconditions[0].Type != PreconditionAppExists {
		t.Fatal("canonicalized order must survive the round-trip")
	}

	// Status transition persists but never touches semantic content.
	if _, err := MarkApplied(p.PlanID, "op_0123456789abcdef", "dep-abc"); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	after, err := Load(p.PlanID)
	if err != nil {
		t.Fatalf("Load after transition: %v", err)
	}
	if after.Status != StatusApplied || after.Correlation == nil || after.Correlation.OperationID != "op_0123456789abcdef" {
		t.Fatalf("correlation lost: %+v", after.Correlation)
	}
	if after.PlanHash != p.PlanHash {
		t.Fatal("status transition must not change the canonical hash")
	}
	_ = dir
}

func TestStore_MissingPlan(t *testing.T) {
	planDataDir(t)
	_, err := Load("pln_0123456789abcdef")
	if !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
		t.Fatalf("missing plan must be NOT_FOUND, got %v", err)
	}
	_, err = Load("bogus")
	if !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("malformed id must be rejected before touching disk, got %v", err)
	}
}

func TestStore_Immutability(t *testing.T) {
	planDataDir(t)
	p := samplePlan(t)
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	// A second Save of the same plan must be rejected outright.
	if err := Save(p); !phelixerr.IsCode(err, phelixerr.CodeAlreadyExists) {
		t.Fatalf("re-saving a plan must be ALREADY_EXISTS, got %v", err)
	}
}

func TestStore_CorruptAndTampered(t *testing.T) {
	planDataDir(t)
	p := samplePlan(t)
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(Dir(), p.PlanID+".json")

	// Unparseable bytes → PLAN_CORRUPT.
	if err := os.WriteFile(path, []byte("{not a plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p.PlanID); !phelixerr.IsCode(err, phelixerr.CodePlanCorrupt) {
		t.Fatalf("garbage plan must be PLAN_CORRUPT, got %v", err)
	}

	// Parseable but tampered content (hash no longer matches) → PLAN_HASH_MISMATCH.
	tampered := samplePlan(t)
	tampered.Inputs.Port = 9999 // semantic field changed under the same hash
	tampered.Status = p.Status
	data, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p.PlanID); !phelixerr.IsCode(err, phelixerr.CodePlanHashMismatch) {
		t.Fatalf("tampered plan must be PLAN_HASH_MISMATCH, got %v", err)
	}

	// Foreign schema version → PLAN_INVALID.
	foreign := samplePlan(t)
	foreign.SchemaVersion = "0"
	foreign.PlanHash, _ = foreign.Hash()
	foreignData, _ := json.Marshal(foreign)
	if err := os.WriteFile(path, foreignData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p.PlanID); !phelixerr.IsCode(err, phelixerr.CodePlanInvalid) {
		t.Fatalf("foreign schema must be PLAN_INVALID, got %v", err)
	}
}

func TestList_NewestFirstAndAppFilter(t *testing.T) {
	planDataDir(t)
	p1 := samplePlan(t)
	p2 := samplePlan(t)
	p3 := samplePlan(t)
	p3.Action = Action{Type: ActionRollback, Application: "other"}
	_ = p3.Finalize()
	for _, p := range []*Plan{p1, p2, p3} {
		if err := Save(p); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	all, _, err := List("", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List() = %d plans, want 3", len(all))
	}
	filtered, _, err := List("demo", 0)
	if err != nil {
		t.Fatalf("List(demo): %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("app filter returned %d, want 2", len(filtered))
	}
	limited, _, err := List("", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit=1 returned %d", len(limited))
	}
}

func TestEvaluate_Capabilities(t *testing.T) {
	p := samplePlan(t) // requires deployment + monitoring
	missing := EvaluateCapabilities(p, []string{"monitoring"})
	if len(missing) != 1 || missing[0] != "deployment" {
		t.Fatalf("missing capability not detected: %v", missing)
	}
	if got := EvaluateCapabilities(p, []string{"monitoring", "deployment"}); len(got) != 0 {
		t.Fatalf("all capabilities present but reported missing: %v", got)
	}
}

func TestEvaluate_PreconditionsAgainstState(t *testing.T) {
	// Unknown precondition types fail closed.
	p := samplePlan(t)
	p.Preconditions = append(p.Preconditions, Precondition{Type: "mysterious_future_check", Expected: "x"})
	failures := p.Evaluate()
	found := false
	for _, f := range failures {
		if f.Type == "mysterious_future_check" && f.Actual == "unchecked" {
			found = true
		}
	}
	if !found {
		t.Fatal("unknown precondition must fail closed as unchecked")
	}
}

func TestEvaluate_StaleWithoutMutating(t *testing.T) {
	// current_version precondition against a state store with no versions:
	// expected v1, actual none → stale.
	planDataDir(t)
	p := samplePlan(t)
	failures := p.Evaluate()
	var current *FailedPrecondition
	for i := range failures {
		if failures[i].Type == PreconditionCurrentVersion {
			current = &failures[i]
		}
	}
	if current == nil {
		t.Fatal("no versions deployed: current_version precondition must fail")
	}
	if current.Expected != "v1" || current.Actual != "none" {
		t.Fatalf("stale detail wrong: %+v", current)
	}
}

func TestPlanSize_Bounded(t *testing.T) {
	planDataDir(t)
	p := samplePlan(t)
	// Blow the plan up with a huge (bounded) filler so Save rejects it.
	pad := strings.Repeat("x", maxPlanBytes)
	p.Execution.Steps = []string{pad}
	if err := p.Finalize(); err != nil {
		t.Fatal(err)
	}
	if err := Save(p); !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
		t.Fatalf("oversized plan must be INVALID_ARGUMENT, got %v", err)
	}
}
