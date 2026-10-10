package cmd

// retention.go — bounded retention for the durable agent-execution stores
// (operation records, plans, sessions), mirroring deploy.PruneVersions and the
// authz decision log's bounded history. It runs best-effort after a new
// durable artifact is created: a prune failure is logged and never breaks the
// originating command, exactly like the operation-record write discipline.
//
// Cross-store reference protection lives HERE, not in the leaf packages,
// because only the command layer may import sessions, plans, ops and authz
// together without a dependency cycle (session already imports plans/ops; the
// leaf packages must not import back). A terminal operation or plan is kept
// while any ACTIVE session still references it, an operation is kept while it is
// a plan's recorded correlation, and a plan is kept while an approval exists for
// it. When in doubt, keep it — a dropped reference only degrades a session's
// Resolve to "missing", never to incorrectness.

import (
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/plans"
	"github.com/abdorrahmani/phelix/internal/session"
)

// pruneOperationRecords bounds the operation-record store after a new record is
// created. In-flight records are protected by ops.Prune itself; here we add the
// cross-store protections (active-session references and plan correlations).
func pruneOperationRecords() {
	protected := map[string]bool{}
	if active, _, _, err := session.List(session.StatusActive, "", 0); err == nil {
		for _, s := range active {
			for _, id := range s.OperationIDs {
				protected[id] = true
			}
		}
	}
	if list, _, err := plans.List("", 0); err == nil {
		for _, p := range list {
			if opID := p.CorrelationOperationID(); opID != "" {
				protected[opID] = true
			}
		}
	}
	if _, err := ops.Prune(0, protected); err != nil {
		logs.WarningFile("ops", "operation retention prune failed: %v", err)
	}
}

// prunePlans bounds the plan store after a new plan is created, protecting
// plans referenced by an active session and plans that carry an approval (a
// deliberate, revocable human action pins the plan it authorizes).
func prunePlans() {
	protected := map[string]bool{}
	if active, _, _, err := session.List(session.StatusActive, "", 0); err == nil {
		for _, s := range active {
			for _, id := range s.PlanIDs {
				protected[id] = true
			}
		}
	}
	if list, _, err := plans.List("", 0); err == nil {
		for _, p := range list {
			if ap, _ := authz.LoadApproval(p.PlanID); ap != nil {
				protected[p.PlanID] = true
			}
		}
	}
	if _, err := plans.Prune(0, protected); err != nil {
		logs.WarningFile("plans", "plan retention prune failed: %v", err)
	}
}

// pruneSessions bounds the session store after a new session is created. It is
// self-contained: only terminal sessions are ever removed, so no cross-store
// protection is needed.
func pruneSessions() {
	if _, err := session.PruneTerminal(0); err != nil {
		logs.WarningFile("session", "session retention prune failed: %v", err)
	}
}
