package cmd

import (
	"time"

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

// Local defense until the deploy package exposes its equivalent shared bound.
const maxRemoteRollbackVerifyDuration = 30 * time.Minute

// RemoteRollback executes a backend-issued rollback command for the
// application named (or identified by ID) in payload.AppName. The payload's
// RequestID rides the rollback telemetry metadata as request_id so the
// backend can tie the event stream to the command that initiated it.
func RemoteRollback(payload monitor.CommandPayload) error {
	requestID := payload.RequestID
	name := payload.AppName
	if name == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "rollback command requires an app")
	}

	// Reason: same validation rules as the local --reason flag. A remote
	// reason is always "explicit" — the backend sent it on purpose.
	reason, err := deploy.ValidateRollbackReason(payload.Reason, payload.Reason != "")
	if err != nil {
		return err
	}

	// Verification: the wire unit is milliseconds (30000 = 30s), converted
	// once at this boundary into the time.Duration the rollback engine uses
	// everywhere else. Zero means "no verification"; negative is invalid.
	var verifyDuration time.Duration
	if payload.VerifyDuration < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid verify_duration_ms %d: must be >= 0", payload.VerifyDuration)
	}
	if payload.VerifyDuration > maxRemoteRollbackVerifyDuration {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid verify_duration_ms %d: maximum is %s", payload.VerifyDuration, maxRemoteRollbackVerifyDuration)
	}
	verifyDuration = payload.VerifyDuration

	// App identity: the same resolution every remote command uses — the
	// daemon's in-memory manager (loaded at startup, kept fresh by the
	// manager itself), resolved by ID or name via GetAppInfo. No second
	// lookup mechanism.
	appInfo, err := GetAppInfo(name)
	if err != nil {
		return err
	}
	name = appInfo.Name

	// Shared resolution path with the local CLI: explicit selector ("v7",
	// "7", tag name) or previous version, plus the rollback-to-current guard.
	target, targetTag, targetSource, err := resolveRollbackTarget(name, payload.Target)
	if err != nil {
		return err
	}

	if payload.DryRun {
		return renderRollbackPreviewWithRequestID(appInfo, name, target, targetTag, targetSource, reason, verifyDuration, requestID)
	}

	state, deployErr := classifyRollbackDeployState(name)
	if deployErr != nil {
		return deployErr
	}
	if state == nil {
		return rollbackClassic(appInfo, name, target, targetTag, targetSource, reason, verifyDuration, requestID)
	}

	// --- Zero-downtime rollback path (blue-green / rolling) ---
	return rollbackZeroDowntime(appInfo, name, target, targetTag, targetSource, state, reason, verifyDuration, requestID)
}
