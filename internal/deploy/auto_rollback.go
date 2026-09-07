package deploy

import (
	"context"
	"fmt"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Automatic rollback: when a forward deployment fails after a new version was
// built and recorded (but NOT on a pure build failure — no new version means
// nothing to roll back from), restore the last known-good version through the
// same ExecuteRollback path a manual rollback takes, so locks, state
// reconciliation, health checks, proxy switches and history are all shared
// rather than duplicated.
//
// Trigger boundaries (enforced here and by the CLI wiring):
//   - build/compile failure before any version was recorded → no rollback;
//     RecoverableDeployFailure classifies the error and the CLI only calls
//     this path with a recorded target version.
//   - deploy-phase failure (instance start, health check, proxy switch,
//     partial rolling rollout) → restore.
//   - recovery itself failing → CodeAutoRollbackFailed; the caller must
//     surface a degraded state and never claim success.
//
// No-op case: blue-green failures always abort BEFORE the traffic switch, so
// the known-good version is still serving and untouched. When the failed
// version is not serving anywhere (blue-green pre-switch abort, rolling
// failure at the first replica), RunAutoRollback reports that and skips the
// redeploy — a rollback that would be a no-op must not be fabricated, and no
// history record is written for a rollback that did not happen.
//
// Re-entrancy / idempotency: RunAutoRollback only ever runs from a failed
// forward deploy, never from a failed rollback (ExecuteRollback has no
// failure path that calls back into this file). After a successful recovery
// the failed version is no longer serving, so a repeated trigger takes the
// no-op path instead of rolling back again.

// AutoRollbackResult summarizes one automatic-recovery attempt for the CLI to
// render and for exit-code mapping.
type AutoRollbackResult struct {
	// FromVer is the failed deployment's version (0 when unknown).
	FromVer int
	// ToVer is the restored known-good version.
	ToVer int
	// Restored is true when the known-good version is serving again — either
	// because recovery redeployed it or because the failure never reached
	// traffic (AlreadyServing). False means the caller MUST report a
	// degraded state, never success.
	Restored bool
	// AlreadyServing marks the no-op case: the known-good version was never
	// displaced, so no rollback was executed.
	AlreadyServing bool
	// Err is the recovery failure when Restored is false.
	Err error
}

// AutoRollbackOptions wires the recovery path. It mirrors the subset of
// RollbackOptions the CLI already assembles for a rollback.
type AutoRollbackOptions struct {
	AppName        string
	AppID          string
	PublicPort     int
	Launcher       InstanceLauncher
	ProxyClient    ProxyClient
	HealthProvider HealthConfigProvider
	Logger         Logger
	Notifier       Notifier
	InFlight       InFlightProvider
	// Replicas is the replica width for rolling-mode recovery; 0 uses state.
	Replicas int
	// FailedVersion is the version the failed deployment tried to promote
	// (the FreshBuildSource target), 0 when the build never completed.
	FailedVersion int
	// FailureReason becomes the history reason, e.g.
	// "Deployment v13 failed: ... <structured deploy error>".
	FailureReason string
}

// AutoRollbackTarget resolves the previous known-good version for a failed
// deploy of newVer, or an error when no safe target exists (the caller then
// reports that recovery was enabled but impossible — it never fabricates a
// target). Only promoted versions qualify, so failed deploys can never be
// chosen; newVer 0 skips the same-version sanity check.
func AutoRollbackTarget(appName string, newVer int) (int, error) {
	target, err := LastKnownGoodVersion(appName)
	if err != nil {
		return 0, err
	}
	if newVer > 0 && target == newVer {
		return 0, phelixerr.Newf(phelixerr.CodeRollbackTargetNotFound,
			"deploy: the only known-good version for %q is the one that just failed (v%d)", appName, newVer)
	}
	return target, nil
}

// RunAutoRollback restores the last known-good version after a failed
// deployment. It acquires the deploy lock itself, so the caller must have
// RELEASED the lock its own deploy held first (same-process flock would
// otherwise deadlock). The recovered state is written by ExecuteRollback's
// normal commit path — no parallel state is introduced.
func RunAutoRollback(ctx context.Context, opts AutoRollbackOptions) AutoRollbackResult {
	log := opts.Logger
	if log == nil {
		log = &nopLogger{}
	}
	target, err := AutoRollbackTarget(opts.AppName, opts.FailedVersion)
	if err != nil {
		return AutoRollbackResult{FromVer: opts.FailedVersion, Err: err}
	}

	// Did the failed version actually reach traffic? A blue-green abort
	// always happens before the switch, and a rolling abort before the first
	// membership swap leaves the old fleet intact — in both cases there is
	// nothing to restore.
	if state, serr := Load(opts.AppName); serr == nil && !failedVersionServing(state, opts.FailedVersion) {
		log.Successf("v%d still serving; the deployment failure did not reach traffic, no rollback needed", target)
		return AutoRollbackResult{
			FromVer:        opts.FailedVersion,
			ToVer:          target,
			Restored:       true,
			AlreadyServing: true,
		}
	}

	log.Stepf("automatic rollback: restoring v%d", target)
	err = ExecuteRollback(ctx, RollbackOptions{
		AppName:        opts.AppName,
		AppID:          opts.AppID,
		PublicPort:     opts.PublicPort,
		TargetVersion:  target,
		Replicas:       opts.Replicas,
		Launcher:       opts.Launcher,
		ProxyClient:    opts.ProxyClient,
		HealthProvider: opts.HealthProvider,
		Logger:         opts.Logger,
		Notifier:       opts.Notifier,
		InFlight:       opts.InFlight,
		Reason:         opts.FailureReason,
		SourceTag:      RollbackSourceAutomatic,
		// Recovery marks this as automatic post-deployment recovery: history
		// records FROM = the failed version, and the rollback-to-current guard
		// is bypassed (a partial rolling rollout legitimately redeploys the
		// version versions.json still names as current).
		Recovery: &RollbackRecovery{FailedVersion: opts.FailedVersion},
	})
	if err != nil {
		return AutoRollbackResult{
			FromVer: opts.FailedVersion,
			ToVer:   target,
			Err: phelixerr.Wrapf(phelixerr.CodeAutoRollbackFailed, err,
				"automatic rollback to v%d failed; previous known-good version could not be restored safely", target),
		}
	}
	log.Successf("v%d started, healthy and serving traffic", target)
	return AutoRollbackResult{FromVer: opts.FailedVersion, ToVer: target, Restored: true}
}

// RollbackRecovery marks a rollback as an automatic post-deployment recovery
// when set on RollbackOptions.
type RollbackRecovery struct {
	// FailedVersion is the deployment that failed and triggered the recovery;
	// recorded as the history FROM version (the version record itself still
	// names the restore target as current, so CurrentVersion cannot supply
	// the failed one).
	FailedVersion int
}

// RecoverableDeployFailure reports whether a deployment error is a
// deployment-phase failure that automatic rollback should respond to. Build
// and toolchain failures (no new version was ever recorded) and user
// interruptions are NOT recoverable — rolling back for them would restore
// state that was never displaced.
func RecoverableDeployFailure(err error) bool {
	if err == nil {
		return false
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return false
	}
	switch phelixerr.CodeOf(err) {
	case phelixerr.CodeInstanceStartFailed, phelixerr.CodeHealthCheckFailed,
		phelixerr.CodeProxy, phelixerr.CodeConnection, phelixerr.CodeDeployFailed:
		return true
	}
	return false
}

// AutoRollbackReason derives the history reason from the deployment failure:
// the structured deploy error the operator already saw, not a parallel
// message. Bounded by MaxRollbackReasonLength like any other history reason.
func AutoRollbackReason(failedVer int, deployErr error) string {
	if failedVer > 0 {
		if deployErr == nil {
			return fmt.Sprintf("Deployment v%d failed", failedVer)
		}
		return truncateReason(fmt.Sprintf("Deployment v%d failed: %s", failedVer, deployErr.Error()))
	}
	if deployErr == nil {
		return "Deployment failed"
	}
	return truncateReason("Deployment failed: " + deployErr.Error())
}

func truncateReason(s string) string {
	r := []rune(s)
	if len(r) <= MaxRollbackReasonLength {
		return s
	}
	return string(r[:MaxRollbackReasonLength])
}

// failedVersionServing reports whether any instance recorded as running in
// the deploy state still runs the failed version — the signal that a partial
// rollout reached traffic and a real redeploy is needed. Blue-green
// candidates killed by the abort have PID 0 and never count.
func failedVersionServing(state *DeployState, failedVer int) bool {
	if state == nil || failedVer <= 0 {
		return false
	}
	serving := func(inst *Instance) bool {
		return inst != nil && inst.PID > 0 && inst.Version == failedVer
	}
	for _, inst := range state.Slots {
		if serving(inst) {
			return true
		}
	}
	for _, inst := range state.Replicas {
		if serving(inst) {
			return true
		}
	}
	return false
}
