package cmd

import (
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/monitor"
)

// Remote rollback: the backend sends a "rollback" MonitorCommandRequest, and
// this handler runs the SAME code path as `phelix rollback` — target
// resolution (resolveRollbackTarget), dry-run planning (renderRollbackPreview)
// and execution (rollbackClassic / rollbackZeroDowntime) are the CLI's own
// functions. There is deliberately no second rollback implementation here:
// this file only translates the command payload into the same calls the CLI
// makes.
//
// Termination semantics: the MonitorCommandResult is sent when this handler
// returns, so a remote rollback is synchronous at the command level — the
// result IS the terminal outcome (success/failed, including any requested
// verification window). The detailed rollback lifecycle travels separately
// through RollbackReporter events, exactly as it does for local and automatic
// rollbacks; the backend must not treat the command result as the only signal
// that a rollback finished.
//
// Idempotency ordering: every error raised BEFORE the rollback engine commits
// (reason/verify validation, app and target resolution, and the authorization
// gate) is wrapped with monitor.NotStarted so the gRPC dispatcher releases the
// durable idempotency key — a refused or never-started command consumes none,
// so a corrected retry under the same request_id executes rather than
// replaying the refusal. This gives the remote path the same guarantee the
// local CLI gets by gating authz before its request-key ledger.

// RemoteRollback executes a backend-issued rollback command for the
// application named (or identified by ID) in payload.AppName. The payload's
// RequestID rides the rollback telemetry metadata as request_id so the
// backend can tie the event stream to the command that initiated it.
func RemoteRollback(payload monitor.CommandPayload) error {
	requestID := payload.RequestID
	name := payload.AppName
	if name == "" {
		return monitor.NotStarted(phelixerr.New(phelixerr.CodeInvalidArgument, "rollback command requires an app"))
	}

	// Reason: same validation rules as the local --reason flag. A remote
	// reason is always "explicit" — the backend sent it on purpose.
	reason, err := deploy.ValidateRollbackReason(payload.Reason, payload.Reason != "")
	if err != nil {
		return monitor.NotStarted(err)
	}

	// Verification: the wire unit is milliseconds, converted to this
	// time.Duration once at the transport boundary (handleMonitorCommand),
	// which is also the single authoritative point that range-checks it
	// against deploy.MaxRollbackVerifyDuration. Only a defensive sign check
	// remains here for direct callers; the upper bound is not re-validated.
	if payload.VerifyDuration < 0 {
		return monitor.NotStarted(phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid verify duration %s: must be >= 0", payload.VerifyDuration))
	}
	verifyDuration := payload.VerifyDuration

	// App identity: the same resolution every remote command uses — the
	// daemon's in-memory manager (loaded at startup, kept fresh by the
	// manager itself), resolved by ID or name via GetAppInfo. No second
	// lookup mechanism.
	appInfo, err := GetAppInfo(name)
	if err != nil {
		return monitor.NotStarted(err)
	}
	name = appInfo.Name

	// Shared resolution path with the local CLI: explicit selector ("v7",
	// "7", tag name) or previous version, plus the rollback-to-current guard.
	target, targetTag, targetSource, err := resolveRollbackTarget(name, payload.Target)
	if err != nil {
		return monitor.NotStarted(err)
	}

	if payload.DryRun {
		return renderRollbackPreviewWithRequestID(appInfo, name, target, targetTag, targetSource, reason, verifyDuration, requestID)
	}

	// The execution authorization boundary (Phase 4). The remote rollback
	// runs in-process inside the monitor daemon rather than re-invoking the
	// CLI, so it needs its own gate call — otherwise a backend-issued
	// rollback would be the one deployment mutation that bypasses the
	// boundary. The dry-run path above is read-only and stays ungated. A
	// denial is wrapped NotStarted so it consumes no idempotency key.
	if authzErr := authorizeDirectExecution(authz.ActionRollback, name, appInfo.ID); authzErr != nil {
		return monitor.NotStarted(authzErr)
	}

	state, deployErr := classifyRollbackDeployState(name)
	if deployErr != nil {
		return monitor.NotStarted(deployErr)
	}
	// From here on the rollback engine takes the deploy lock and may mutate
	// state: its errors are NOT NotStarted — a retry must replay, not re-run.
	if state == nil {
		return rollbackClassic(appInfo, name, target, targetTag, targetSource, reason, verifyDuration, requestID, nil)
	}

	// --- Zero-downtime rollback path (blue-green / rolling) ---
	return rollbackZeroDowntime(appInfo, name, target, targetTag, targetSource, state, reason, verifyDuration, requestID, nil)
}
