package cmd

// authz_gate.go — where Phelix's execution authorization boundary meets the
// command layer.
//
// Phase 4's architectural invariant is that a protected mutation cannot reach
// the execution engine without passing the boundary. This file is the only
// place the command layer knows about authorization, and it exposes exactly
// two entry points:
//
//	authorizePlanExecution — plan-bound execution (phelix plan apply)
//	authorizeDirectExecution — planless execution (phelix rebuild/rollback,
//	                           and therefore also the webhook and remote
//	                           command paths, which re-invoke those commands)
//
// Both call the same [authz.Gate]; neither contains an authorization rule.
// The rules live in internal/authz, the deployment semantics live where they
// always have, and nothing here decides anything.
//
// Which entry points are protected, and why these:
//
//	phelix plan apply   — the machine-facing execution path (Phase 3)
//	phelix rebuild      — the deploy path; ALSO the path the webhook queue and
//	                      the monitor daemon's remote rebuild take, because
//	                      both re-invoke `phelix rebuild` as a subprocess
//	phelix rollback     — the rollback path
//	RemoteRollback      — the backend-issued rollback, which runs in-process
//	                      in the daemon and so needs its own gate call
//
// Deliberately NOT gated in Phase 4: `build` (produces an artifact without
// serving it), the process lifecycle commands (`start`/`stop`/`restart`,
// which do not change the deployed version), and every read-only path
// including `rollback --dry-run` and `--list`. See the authorization guide;
// extending the boundary is a later phase's decision, not an accident of this
// file.

import (
	"context"

	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/plans"
)

// authzGate is the process-wide boundary. It is a var so the mutation-safety
// tests can drive every decision path without writing a policy to disk;
// production never replaces it.
var authzGate = defaultAuthzGate()

func defaultAuthzGate() *authz.Gate {
	g := authz.DefaultGate()
	// An audit-write failure is reported, never obeyed: it must not turn an
	// authorized execution into a failure, and it must not turn a denial into
	// an allow. The decision has already been made when this runs.
	g.OnRecordError = func(err error) {
		logs.WarningFile("authz", "authorization decision not recorded: %v", err)
	}
	return g
}

// authorizePlanExecution is the gate for applying an immutable plan. It is
// called at the point mutation is about to begin — after the plan has been
// loaded, hash-verified, precondition-checked, capability-checked and
// re-resolved against the live execution path, and immediately before the
// execution seam — so the decision cannot go stale between being made and
// being acted on.
//
// Authorization binds to the plan's ID *and* hash. A decision made for plan A
// therefore cannot permit plan B, and a plan whose content changed no longer
// matches the approval it was granted.
func authorizePlanExecution(plan *plans.Plan) (authz.Outcome, error) {
	return authzGate.Authorize(context.Background(), authz.ExecutionRequest{
		Action: plan.Action.Type,
		Target: authz.Target{App: plan.Action.Application, AppID: plan.Target.AppID},
		Plan:   &authz.PlanRef{ID: plan.PlanID, Hash: plan.PlanHash},
	})
}

// authorizeDirectExecution is the gate for a planless mutation — a direct
// `phelix rebuild`/`rollback`, or anything that re-invokes them.
//
// On a host that has not configured authorization this is the legacy local
// path and behaves exactly as it always has. Under enforced authorization it
// is denied: protected execution must be bound to an immutable plan, because
// authorizing a bare action name would let the executed content differ from
// whatever was authorized. That is the whole reason plans exist.
//
// The gate is called before the operation record and before the request-key
// ledger entry, so a refusal consumes no idempotency key and leaves no
// operation behind — the same request succeeds unchanged once it is
// authorized.
func authorizeDirectExecution(action, app, appID string) error {
	_, err := authzGate.Authorize(context.Background(), authz.ExecutionRequest{
		Action: action,
		Target: authz.Target{App: app, AppID: appID},
		Plan:   nil,
	})
	return err
}
