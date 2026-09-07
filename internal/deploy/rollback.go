package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// DetectGitCommit returns the HEAD commit hash for projectRoot when git is
// available; empty string otherwise.
func DetectGitCommit(projectRoot string) string {
	if projectRoot == "" {
		return ""
	}
	cmd := exec.Command("git", "-C", projectRoot, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// rollbackLogPath returns ~/.phelix/apps/<AppName>/rollback.log.
func rollbackLogPath(appName string) (string, error) {
	dir, err := appDataDir(appName)
	if err != nil {
		return "", err
	}
	return dir + "/rollback.log", nil
}

// appendRollbackLog writes a human-readable line to the per-app rollback
// audit log. Best-effort: errors are silently ignored so a log failure never
// blocks the rollback itself.
func appendRollbackLog(appName string, ev RollbackEvent) {
	path, err := rollbackLogPath(appName)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	status := "ok"
	if !ev.Success {
		status = "FAILED: " + ev.ErrMsg
	}
	line := fmt.Sprintf("%s rollback v%d -> v%d %s\n",
		ev.Timestamp.Format(time.RFC3339), ev.FromVer, ev.ToVer, status)
	_, _ = f.WriteString(line)
}

// RollbackOptions configures a rollback that reuses blue-green or rolling Deploy.
type RollbackOptions struct {
	AppName    string
	AppID      string
	PublicPort int
	// TargetVersion is 0 for "previous", or explicit vN.
	TargetVersion int
	Replicas      int // for rolling mode; 0 = use state

	Source         BuildSource // if nil, ExistingVersionSource is constructed
	Launcher       InstanceLauncher
	ProxyClient    ProxyClient
	HealthProvider HealthConfigProvider
	Logger         Logger
	Notifier       Notifier
	InFlight       InFlightProvider
	// Telemetry observes the rollback as a deployment. Optional: a nil Tracker
	// is silent.
	Telemetry *Tracker
	// Reason is the operator-supplied explanation persisted with the rollback
	// history record. Optional; validated by ValidateRollbackReason.
	Reason string
	// Verify, when non-nil, requests post-rollback stability observation: the
	// serving instances are probed for the requested duration after the
	// rollback's state commit, and the outcome is recorded in history.
	Verify *VerifyRequest
}

// VerifyRequest configures post-rollback stability verification.
type VerifyRequest struct {
	// Duration is the observation window (the whole window, not one probe).
	Duration time.Duration
	// OnTick, when set, receives a progress callback per observation sweep;
	// the CLI renders progress lines from it.
	OnTick func(elapsed time.Duration, err error)
}

// ExecuteRollback runs rollback through the same zero-downtime path as forward
// deploy. It does not symlink-flip in isolation — that would bypass health
// checks and leave DeployState inconsistent with the proxy.
func ExecuteRollback(ctx context.Context, opts RollbackOptions) error {
	if opts.AppName == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: rollback requires app name")
	}
	fromVer, _ := CurrentVersion(opts.AppName)

	src := opts.Source
	if src == nil {
		src = &ExistingVersionSource{AppName: opts.AppName, Version: opts.TargetVersion}
	}
	toVer := opts.TargetVersion
	if ev, ok := src.(*ExistingVersionSource); ok {
		toVer = ev.TargetVersion()
	} else if va, ok := src.(VersionAwareSource); ok {
		toVer = va.TargetVersion()
	}
	if toVer <= 0 {
		return phelixerr.Newf(phelixerr.CodeRollbackTargetNotFound, "deploy: could not resolve rollback target version for %q", opts.AppName)
	}
	if fromVer > 0 && toVer == fromVer {
		return phelixerr.Newf(phelixerr.CodeRollbackTargetNotFound, "deploy: already running v%d; nothing to roll back to", fromVer)
	}

	release, err := AcquireDeployLock(opts.AppName, "rollback")
	if err != nil {
		return err
	}
	defer release()

	log := opts.Logger
	if log == nil {
		log = &nopLogger{}
	}

	state, err := Load(opts.AppName)
	if err != nil {
		if os.IsNotExist(err) {
			return phelixerr.Newf(
				phelixerr.CodeRollbackTargetNotFound,
				"deploy: no deploy state for %q — rollback requires a prior zero-downtime deploy",
				opts.AppName,
			)
		}
		return err
	}

	// Instances from an aborted predecessor may still be recorded as running
	// with PIDs that no longer exist. Clear them so the rollback starts from
	// records that describe reality and cannot inherit stale PIDs.
	ReapStaleInstances(state)

	ev := RollbackEvent{
		AppName:   opts.AppName,
		FromVer:   fromVer,
		ToVer:     toVer,
		Timestamp: time.Now(),
	}
	mode := string(state.Mode)
	var deployErr error
	// verify is filled in only on the success path below (execution failures
	// never reach verification); the deferred history write picks it up so the
	// record is written once, at the single terminal point, with the full
	// outcome.
	var verify *RollbackVerification
	defer func() {
		ev.Success = deployErr == nil
		if deployErr != nil {
			ev.ErrMsg = deployErr.Error()
		}
		msg := FormatRollbackEvent(ev)
		log.Infof("%s", msg)
		if opts.Notifier != nil {
			_ = opts.Notifier.Notify(ctx, msg)
		}
		// Structured history record: written for BOTH outcomes, at the single
		// terminal point of the rollback transaction, with the mode actually
		// used. A failed rollback must never end up recorded as success.
		RecordRollbackResult(opts.AppName, fromVer, toVer, mode, opts.Reason, verify, deployErr)
	}()

	switch state.Mode {
	case ModeBlueGreen:
		bg := &BlueGreen{
			AppName:        opts.AppName,
			AppID:          opts.AppID,
			PublicPort:     opts.PublicPort,
			Source:         src,
			Launcher:       opts.Launcher,
			ProxyClient:    opts.ProxyClient,
			HealthProvider: opts.HealthProvider,
			Logger:         log,
			Notifier:       opts.Notifier,
			InFlight:       opts.InFlight,
			Telemetry:      opts.Telemetry,
		}
		if opts.PublicPort == 0 {
			bg.PublicPort = state.PublicPort
		}
		deployErr = bg.Deploy(ctx)
	case ModeRolling:
		replicas := opts.Replicas
		if replicas <= 0 {
			replicas = len(state.Replicas)
		}
		if replicas < 1 {
			replicas = 1
		}
		r := &Rolling{
			AppName:        opts.AppName,
			AppID:          opts.AppID,
			PublicPort:     state.PublicPort,
			Replicas:       replicas,
			Source:         src,
			Launcher:       opts.Launcher,
			ProxyClient:    opts.ProxyClient,
			HealthProvider: opts.HealthProvider,
			Logger:         log,
			Notifier:       opts.Notifier,
			InFlight:       opts.InFlight,
			Telemetry:      opts.Telemetry,
		}
		deployErr = r.Deploy(ctx)
	default:
		deployErr = phelixerr.Newf(phelixerr.CodeRollbackFailed, "deploy: rollback unsupported for mode %q", state.Mode)
	}
	if deployErr != nil {
		// "active instance untouched" is only claimed on this path because the
		// blue-green/rolling rollback abort leaves the previous instance active
		// (the deploy layer already guarantees that on failure).
		return phelixerr.Wrapf(phelixerr.CodeRollbackFailed, deployErr, "rollback failed; active instance untouched")
	}

	// Record the successful rollback in state and audit log. The deploy path
	// re-persisted the full state with the post-rollback reality (new PIDs,
	// ports, active slot/version); reload it rather than mutating the stale
	// pre-rollback snapshot — writing that snapshot back was clobbering the
	// fresh records and resurrecting dead PIDs as "running", which made list/
	// status contradict the live instances and the proxy.
	state, err = Load(opts.AppName)
	if err != nil {
		log.Warnf("failed to reload deploy state after rollback: %v", err)
	} else {
		state.LastRollback = &RollbackRecord{
			FromVersion: fromVer,
			ToVersion:   toVer,
			At:          ev.Timestamp,
		}
		if err := Store(state); err != nil {
			log.Warnf("failed to persist rollback record: %v", err)
		}
	}
	appendRollbackLog(opts.AppName, ev)

	// Post-rollback stability verification. Runs only after the authoritative
	// state commit above, so it observes exactly the version/slot/replicas now
	// serving. It stays inside the deploy lock deliberately: the lock only
	// excludes concurrent deploy/rollback mutations (reads never take it), so
	// holding it guarantees a second rollout cannot invalidate the observation
	// window undetected. ponytail: no invalidation-detection protocol — add
	// one if a mid-verification emergency deploy ever needs to be allowed.
	if opts.Verify != nil {
		verr := VerifyRollbackStability(ctx, VerificationOptions{
			AppName:        opts.AppName,
			AppID:          opts.AppID,
			Duration:       opts.Verify.Duration,
			HealthProvider: opts.HealthProvider,
			Logger:         log,
			OnTick:         opts.Verify.OnTick,
		})
		verify = &RollbackVerification{
			Requested: true,
			Duration:  opts.Verify.Duration.String(),
			At:        time.Now(),
		}
		switch {
		case verr == nil:
			verify.Status = RollbackVerifyPassed
		case errors.Is(verr, context.Canceled):
			verify.Status = RollbackVerifyCancelled
			verify.Error = verr.Error()
			log.Warnf("rollback verification cancelled; rollback to v%d remains active", toVer)
			return phelixerr.Newf(phelixerr.CodeRollbackVerifyFailed,
				"rollback verification cancelled; rollback to v%d remains active", toVer)
		default:
			verify.Status = RollbackVerifyFailed
			verify.Error = verr.Error()
			log.Errorf("rollback verification failed: %v", verr)
			return phelixerr.Wrapf(phelixerr.CodeRollbackVerifyFailed, verr,
				"rollback to v%d completed, but the application did not remain healthy during the %s verification window",
				toVer, opts.Verify.Duration)
		}
	}
	return nil
}
