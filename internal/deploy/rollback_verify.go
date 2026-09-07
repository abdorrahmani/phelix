package deploy

import (
	"context"
	"fmt"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// Post-rollback stability verification: observe the instance(s) actually
// serving traffic for a requested duration, reusing the same tiered health
// checks and per-app DeployTierConfig the deploy itself used. The sampling
// interval and probe semantics come from the health package (DefaultTierInterval
// and resolvedConfig); the verification duration is only the observation
// window, never a probe timeout.
//
// The runner reports; it never mutates: no process restarts, no proxy
// changes, no second rollback. Recovery after a failed verification is an
// explicit operator decision.

// VerifyTarget names one probe destination for stability verification. Used
// directly when VerificationOptions.Targets is set (classic rollbacks, which
// have no DeployState); otherwise targets are derived from the committed
// DeployState.
type VerifyTarget struct {
	Label    string
	HostPort string // "127.0.0.1:PORT"
	PID      int
}

// VerificationOptions configures one stability observation pass.
type VerificationOptions struct {
	// AppName selects the persisted DeployState whose serving instances are
	// observed. The state is re-loaded here (not trusted from before the
	// rollback) so verification always targets the post-commit reality.
	AppName string
	// AppID resolves the per-app deploy-tier health config.
	AppID string
	// Duration is how long the app must remain healthy.
	Duration time.Duration
	// HealthProvider supplies the deploy-tier config; nil means auto.
	HealthProvider HealthConfigProvider
	// Targets, when set, overrides the state-derived probe targets — used by
	// classic rollbacks, which have no DeployState. The tier is auto-selected
	// for the first target.
	Targets []VerifyTarget
	// Logger receives progress lines; nil is silent.
	Logger Logger
	// OnTick fires after each observation sweep with time since the window
	// started and, once the app has gone unhealthy, that error. nil is fine;
	// the CLI uses it for the "5s ✓ healthy" progress lines.
	OnTick func(elapsed time.Duration, err error)
}

// VerifyRollbackStability observes the app's serving instances for
// opts.Duration. nil means the app stayed healthy the whole window; a
// structured error (CodeHealthCheckFailed) means it did not. The context only
// cancels the observation — callers must translate cancellation themselves
// (a Ctrl+C here is NOT a rollback failure, since execution already
// committed).
//
// Traffic-awareness: targets come from the freshly loaded DeployState — the
// blue-green active slot or all running replicas — which is the same state the
// deploy just committed, so observation always covers what actually serves,
// never the drained slot.
func VerifyRollbackStability(ctx context.Context, opts VerificationOptions) error {
	if opts.Duration <= 0 {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: verification duration must be positive")
	}
	log := opts.Logger
	if log == nil {
		log = &nopLogger{}
	}
	tierCfg := tierConfig(opts)

	var targets []verifyTarget
	state, err := Load(opts.AppName)
	if err != nil || state == nil {
		if len(opts.Targets) == 0 {
			return phelixerr.Wrapf(phelixerr.CodeInvalidArgument, err,
				"deploy: stability verification requires a zero-downtime deploy state for %q", opts.AppName)
		}
		// Classic path: explicit targets, tier auto-selected like a deploy.
		tier := health.SelectTier(tierCfg, opts.Targets[0].HostPort, nil)
		targets = make([]verifyTarget, len(opts.Targets))
		for i, t := range opts.Targets {
			targets[i] = verifyTarget{label: t.Label, addr: t.HostPort, pid: t.PID, tier: tier}
		}
		log.Infof("verification tier selected: %s", tier)
	} else {
		targets = servingTargets(state)
		// The tier recorded by the deploy is reused when present; otherwise it
		// is resolved the same way the deploy resolves it (SelectTier against
		// the first serving address).
		if state.Health == nil || state.Health.Tier <= 0 {
			tier := health.SelectTier(tierCfg, targets[0].addr, nil)
			for i := range targets {
				targets[i].tier = tier
			}
			log.Infof("verification tier selected: %s", tier)
		}
	}
	if len(targets) == 0 {
		return phelixerr.Newf(phelixerr.CodeHealthCheckFailed,
			"deploy: no running instance recorded for %q; nothing to verify", opts.AppName)
	}

	start := time.Now()
	deadline := start.Add(opts.Duration)
	// Sample at the deploy default interval; windows shorter than one
	// interval sample faster so the deadline is honored promptly.
	tick := health.DefaultTierInterval
	if opts.Duration < tick {
		tick = opts.Duration
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	var failErr error
	for {
		// One sweep over every instance currently serving traffic; the first
		// failing probe is remembered and reported, but observation continues
		// to the end of the window so the operator sees how long (and that)
		// the app stayed unhealthy.
		for _, tgt := range targets {
			if failErr == nil && !health.Check(ctx, tgt.tier, tierCfg, tgt.addr, tgt.pid, nil) {
				failErr = phelixerr.Newf(phelixerr.CodeHealthCheckFailed,
					"instance %s (pid %d, %s) is unhealthy", tgt.label, tgt.pid, tgt.addr)
			}
		}
		elapsed := time.Since(start)
		if time.Now().After(deadline) {
			return failErr
		}
		if opts.OnTick != nil {
			opts.OnTick(elapsed, failErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// tierConfig resolves the deploy-tier health config for verification from the
// same provider the deploy used.
func tierConfig(opts VerificationOptions) *health.DeployTierConfig {
	if opts.HealthProvider != nil {
		return opts.HealthProvider(opts.AppID)
	}
	return DefaultHealthProvider()(opts.AppID)
}

// verifyTarget is one probe destination: the tier selected at deploy time is
// reused so observation matches what "healthy" meant during the rollout.
type verifyTarget struct {
	label string
	addr  string
	pid   int
	tier  health.Tier
}

// servingTargets maps the committed DeployState onto probe targets. Blue-green
// contributes only the ACTIVE slot (never the drained one); rolling
// contributes every running replica.
func servingTargets(state *DeployState) []verifyTarget {
	tier := health.TierUnknown
	if state.Health != nil && state.Health.Tier > 0 {
		tier = health.Tier(state.Health.Tier)
	}
	var out []verifyTarget
	add := func(label string, inst *Instance) {
		if inst == nil || inst.PID <= 0 || inst.Port <= 0 || inst.Status != "running" {
			return
		}
		out = append(out, verifyTarget{
			label: label,
			addr:  hostPort(inst.Port),
			pid:   inst.PID,
			tier:  tier,
		})
	}
	switch state.Mode {
	case ModeBlueGreen:
		add("slot "+state.ActiveSlot, state.ActiveInstance())
	case ModeRolling:
		for _, k := range replicaIndices(state.Replicas) {
			add("replica "+k, state.Replicas[k])
		}
	}
	return out
}

// FormatVerifyProgress renders one progress line ("  5s   ✓ healthy" /
// " 15s   ✗ <reason>") following the existing color convention; coloring is
// applied by the caller.
func FormatVerifyProgress(elapsed time.Duration, err error) string {
	mark := "✓"
	text := "healthy"
	if err != nil {
		mark = "✗"
		text = err.Error()
	}
	return fmt.Sprintf("  %-4s %s %s", elapsed.Round(time.Second), mark, text)
}
