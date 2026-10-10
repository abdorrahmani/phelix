package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/plans"
)

func makePlan(t *testing.T) *plans.Plan {
	t.Helper()
	p := &plans.Plan{
		Action: plans.Action{Type: plans.ActionRebuild, Application: "billing"},
		Target: plans.Target{AppName: "billing"},
		Status: plans.StatusCreated,
	}
	if err := p.Finalize(); err != nil {
		t.Fatalf("plan Finalize: %v", err)
	}
	if err := plans.Save(p); err != nil {
		t.Fatalf("plans.Save: %v", err)
	}
	return p
}

func TestReferenceValidationAndResolution(t *testing.T) {
	isolate(t)
	p := makePlan(t)
	rec, err := ops.Begin("rebuild", "billing", "")
	if err != nil {
		t.Fatalf("ops.Begin: %v", err)
	}
	s := mustCreate(t, CreateOpts{App: "billing"})

	// Valid references of every kind link in one checkpoint.
	if _, err := Checkpoint(s.SessionID, CheckpointOpts{
		Note:      "linked",
		Refs:      Refs{Plans: []string{p.PlanID}, Operations: []string{rec.ID}, Deployments: []string{"dep-abcdef0123456789"}},
		ExpectRev: -1,
	}); err != nil {
		t.Fatalf("checkpoint with valid refs: %v", err)
	}

	// Invalid references are rejected up front with INVALID_ARGUMENT, and leave
	// the session unchanged (validated before the write).
	bad := []Refs{
		{Plans: []string{"pln_ffffffffffffffff"}},
		{Operations: []string{"op_ffffffffffffffff"}},
		{Plans: []string{"not-a-plan"}},
		{Deployments: []string{"nope"}},
	}
	for _, b := range bad {
		if _, err := Checkpoint(s.SessionID, CheckpointOpts{Refs: b, ExpectRev: -1}); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
			t.Fatalf("bad ref %+v = %v, want INVALID_ARGUMENT", b, phelixerr.CodeOf(err))
		}
	}

	// Resolution reports present entities and never fabricates a deployment
	// that has no correlating operation record (unknown, not present).
	res, err := Show(s.SessionID, true)
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if len(res.Resolved.Plans) != 1 || !res.Resolved.Plans[0].Present || res.Resolved.Plans[0].State != RefPresent {
		t.Fatalf("plan resolution: %+v", res.Resolved.Plans)
	}
	if len(res.Resolved.Operations) != 1 || !res.Resolved.Operations[0].Present {
		t.Fatalf("operation resolution: %+v", res.Resolved.Operations)
	}
	if len(res.Resolved.Deployments) != 1 || res.Resolved.Deployments[0].State != RefUnknown {
		t.Fatalf("deployment resolution: %+v (want unknown — no dep-id store)", res.Resolved.Deployments)
	}
}

func TestResolveMissingPlanIsHonest(t *testing.T) {
	isolate(t)
	p := makePlan(t)
	s := mustCreate(t, CreateOpts{})
	if _, err := Checkpoint(s.SessionID, CheckpointOpts{Refs: Refs{Plans: []string{p.PlanID}}, ExpectRev: -1}); err != nil {
		t.Fatal(err)
	}
	// Delete the plan after linking; resolution must say missing, not present.
	if err := os.Remove(filepath.Join(plans.Dir(), p.PlanID+".json")); err != nil {
		t.Fatal(err)
	}
	res, err := Show(s.SessionID, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Resolved.Plans[0].Present || res.Resolved.Plans[0].State != RefMissing {
		t.Fatalf("missing plan resolution: %+v", res.Resolved.Plans[0])
	}
}

func TestTooManyReferencesRejected(t *testing.T) {
	isolate(t)
	s := mustCreate(t, CreateOpts{})
	var deps []string
	for i := 0; i <= MaxRefsPerKind; i++ {
		deps = append(deps, fmt.Sprintf("dep-%016x", i))
	}
	if _, err := Checkpoint(s.SessionID, CheckpointOpts{Refs: Refs{Deployments: deps}, ExpectRev: -1}); phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("over-cap references = %v, want INVALID_ARGUMENT", phelixerr.CodeOf(err))
	}
}
