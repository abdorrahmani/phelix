package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Canary and progressive rollout engine.
//
// A rollout runs on the blue-green topology: the stable version keeps serving
// from the active slot while the new version is deployed to the inactive slot
// (the canary) and receives a configurable, gradually increasing share of
// traffic through the proxy's weighted routing. Every step verifies the canary
// — deploy-tier health probes plus, when the proxy daemon exposes per-backend
// metrics, an error-rate and latency comparison against the stable baseline —
// before the next step begins. The final 100% step is the promotion: traffic
// switches fully to the canary, the version is promoted durably and the old
// stable instance drains.
//
// A one-shot canary (`phelix rebuild <app> --canary 5`) is a two-step
// progressive rollout: hold at 5% for the verification window, then promote.
//
// Safety invariants, enforced by the ordering in Deploy:
//   - Traffic is never routed to the canary before it passed the same
//     deploy-tier health gate blue-green uses.
//   - Any failure after the first weighted switch restores the stable version
//     to 100% of traffic before returning, and the canary instance is stopped.
//   - A rollout interrupted by a crash leaves the Canary record in status
//     "running"; the next rollout restores stable routing before it touches
//     anything, so an interrupted rollout always converges on stable.
//   - The deployment is only reported successful after the final promotion is
//     durable: state persisted, version promoted, old instance drained.

// Strategy names for canary-style rollouts. They extend the existing strategy
// vocabulary (classic, blue-green, rolling); the topology they run on is
// blue-green, which is what deploy.json records.
const (
	StrategyCanary      = "canary"
	StrategyProgressive = "progressive"
)

// Defaults for rollout plans. A one-shot canary uses DefaultCanaryPercent for
// its traffic share and DefaultCanaryVerification as its observation window
// unless phelix.yaml configures otherwise; progressive steps always come from
// configuration, never from these constants.
const (
	DefaultCanaryPercent        = 10
	DefaultCanaryVerification   = 30 * time.Second
	DefaultRolloutVerifyTimeout = 10 * time.Second

	DefaultRolloutVerifyInterval = 5 * time.Second
	DefaultMaxCanaryErrorRate    = 5.0 // percent of canary requests
	DefaultMaxCanaryErrorDelta   = 2.0 // percentage points over the baseline
	DefaultCanaryP95Factor       = 3.0 // canary p95 at most N × baseline p95
)

// RolloutStep is one traffic share the canary must survive before the rollout
// proceeds. Duration is how long the canary is observed at this share; zero
// verifies a single health sweep and promotes immediately.
type RolloutStep struct {
	TrafficPercent int
	Duration       time.Duration
}

// VerificationConfig bounds how far the canary may deviate from the stable
// baseline during a step's observation window. Zero values take the defaults
// above.
type VerificationConfig struct {
	// Interval is the health-probe cadence during a step window.
	Interval time.Duration
	// MaxErrorRate is the absolute canary error-rate cap, in percent.
	MaxErrorRate float64
	// MaxErrorDelta is how many percentage points the canary error rate may
	// exceed the stable baseline error rate.
	MaxErrorDelta float64
	// MaxP95Factor bounds the canary's p95 latency relative to the baseline's.
	MaxP95Factor float64
}

func (v VerificationConfig) withDefaults() VerificationConfig {
	out := v
	if out.Interval <= 0 {
		out.Interval = DefaultRolloutVerifyInterval
	}
	if out.MaxErrorRate <= 0 {
		out.MaxErrorRate = DefaultMaxCanaryErrorRate
	}
	if out.MaxErrorDelta <= 0 {
		out.MaxErrorDelta = DefaultMaxCanaryErrorDelta
	}
	if out.MaxP95Factor <= 0 {
		out.MaxP95Factor = DefaultCanaryP95Factor
	}
	return out
}

// RolloutPlan is a validated, engine-ready rollout: the steps in order and the
// verification bounds. The final step must be the 100% promotion.
type RolloutPlan struct {
	Strategy     string
	Steps        []RolloutStep
	Verification VerificationConfig
}

// Validate enforces the plan's structural safety rules: at least a canary step
// plus the final promotion, traffic shares within 1-100, strictly increasing,
// ending at 100%. The CLI validates the same rules from phelix.yaml with
// yaml-path error messages; this is the engine's defensive backstop.
func (p RolloutPlan) Validate() error {
	if p.Strategy == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "rollout: strategy is required")
	}
	if len(p.Steps) < 2 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"rollout: at least 2 steps are required (a canary share and the final 100%% promotion), got %d", len(p.Steps))
	}
	prev := 0
	for i, s := range p.Steps {
		if s.TrafficPercent < 1 || s.TrafficPercent > 100 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"rollout: step %d has traffic %d%%; each step must be between 1%% and 100%%", i+1, s.TrafficPercent)
		}
		if s.TrafficPercent <= prev {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"rollout: step %d has traffic %d%% but the previous step was %d%%; traffic must strictly increase",
				i+1, s.TrafficPercent, prev)
		}
		if s.Duration < 0 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"rollout: step %d has negative duration %s", i+1, s.Duration)
		}
		prev = s.TrafficPercent
	}
	if last := p.Steps[len(p.Steps)-1].TrafficPercent; last != 100 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"rollout: the final step must be the 100%% promotion, got %d%%", last)
	}
	return nil
}

// CanaryPlan builds the two-step plan behind a one-shot canary deploy: hold at
// percent for the verification window, then promote to 100%.
func CanaryPlan(percent int, verification time.Duration, v VerificationConfig) RolloutPlan {
	return RolloutPlan{
		Strategy:     StrategyCanary,
		Steps:        []RolloutStep{{TrafficPercent: percent, Duration: verification}, {TrafficPercent: 100}},
		Verification: v,
	}
}

// StatsClient is the optional per-backend metrics extension of ProxyClient,
// implemented by the real proxy client. Rollouts degrade to health-only
// verification when it is absent (older daemon, offline fakes).
type StatsClient interface {
	Stats(ctx context.Context, appName string) ([]proxy.BackendStat, error)
}

// Rollout holds the dependencies and configuration for one canary/progressive
// deployment. It reuses the same building blocks as blue-green: BuildSource,
// InstanceLauncher, the deploy-tier health config, the proxy control client,
// the version store and the deployment lock (acquired by the CLI caller).
type Rollout struct {
	AppName    string
	AppID      string
	PublicPort int
	Plan       RolloutPlan
	ExtraArgs  []string

	Source         BuildSource
	Launcher       InstanceLauncher
	ProxyClient    ProxyClient
	HealthProvider HealthConfigProvider
	Logger         Logger
	Notifier       Notifier
	InFlight       InFlightProvider
	GracePeriod    time.Duration
	// Runtime records how instances are launched ("native"/"docker"). Persisted
	// into DeployState so rollback and recovery resolve the matching launcher.
	// Empty is treated as native.
	Runtime string
	// PortHandoff, when set, is invoked right before the first proxy
	// enrolment if something still owns the public port (a classic instance
	// from before the app came under the proxy). The CLI wires this to
	// "gracefully stop the app's own classic process"; the canary is already
	// healthy at that point, mirroring blue-green's handoff.
	PortHandoff func(ctx context.Context, appName string, publicPort int) error
	// Telemetry observes the rollout lifecycle. Optional: nil is silent.
	Telemetry *Tracker
	// stateStore is a test seam for deterministic persistence-failure
	// coverage. Production leaves it nil and uses Store.
	stateStore func(*DeployState) error
	// Verify observes the canary during one step's window. nil uses the
	// default health+metrics verifier (verifyStep). Tests inject fakes to
	// make step outcomes deterministic.
	Verify StepVerifier

	// enrolled tracks whether the app is registered with the proxy daemon so
	// the first traffic switch Adds instead of Switch-ing against an unknown
	// app (which silently routes nothing).
	enrolled bool
	// statsBroken disables per-backend metric sampling for the rest of the
	// rollout once the proxy answers that it cannot serve them.
	statsBroken bool
	statsWarned bool
}

// Deploy performs one canary/progressive rollout.
func (ro *Rollout) Deploy(ctx context.Context) (errRet error) {
	log := ro.logger()
	grace := ro.GracePeriod
	if grace <= 0 {
		grace = DefaultGracePeriod
	}
	// Rollout reports failures from many points; one deferred hook keeps the
	// telemetry's terminal event identical to what Deploy actually returned.
	defer func() { reportDeployFailure(ro.Telemetry, errRet) }()

	ro.Telemetry.Started(activeVersionOf(nil, ro.AppName), 0,
		fmt.Sprintf("%s rollout of %s over %d steps", ro.Plan.Strategy, ro.AppName, len(ro.Plan.Steps)))

	if err := ro.Plan.Validate(); err != nil {
		return ro.failf(err)
	}
	if ro.Source == nil {
		return ro.failf(phelixerr.New(phelixerr.CodeInvalidArgument, "rollout: no BuildSource configured"))
	}
	if ro.Launcher == nil {
		return ro.failf(phelixerr.New(phelixerr.CodeInvalidArgument, "rollout: no Launcher configured"))
	}
	if ro.ProxyClient == nil {
		return ro.failf(phelixerr.New(phelixerr.CodeProxy, "rollout: no proxy client (is 'phelix proxy' running?)"))
	}

	// 1. A rollout needs an existing deployment as its stable baseline: there
	// is nothing to canary against — and nothing to fall back to — on a
	// first-ever deploy.
	state, err := Load(ro.AppName)
	if err != nil {
		if os.IsNotExist(err) {
			return ro.failf(phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"rollout: %q has no existing deployment to use as the stable baseline\nHint: deploy once with 'phelix rebuild %s --blue-green' (or --replicas N) before using a canary rollout",
				ro.AppName, ro.AppName))
		}
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "rollout: load deploy state"))
	}
	if state.Mode != ModeBlueGreen && state.Mode != ModeRolling {
		return ro.failf(phelixerr.Newf(phelixerr.CodeConfiguration, "rollout: unsupported deploy mode %q for %q", state.Mode, ro.AppName))
	}
	state.AppID = ro.AppID
	if ro.Runtime != "" {
		state.Runtime = ro.Runtime
	}
	ro.Telemetry.Bind(state)
	ro.Telemetry.SetVersions(activeVersionOf(state, ro.AppName), 0)

	pre := captureMode(state)
	if pre.mode != "" && pre.mode != ModeBlueGreen {
		log.Stepf("strategy migration: %s → %s rollout (previous strategy's instances retire after the first traffic switch)", pre.mode, ro.Plan.Strategy)
	}

	// 2. Resolve the stable baseline BEFORE mutating any state.
	var stable *Instance
	switch pre.mode {
	case ModeRolling:
		stable = firstLiveReplica(state)
		if stable == nil {
			return ro.failf(phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"rollout: the rolling deployment of %q has no live replica to use as the stable baseline\nHint: rebuild with --strategy rolling first", ro.AppName))
		}
	case ModeBlueGreen:
		stable = state.Slots[state.ActiveSlot]
	}
	if stable == nil || stable.Port <= 0 || !InstanceAlive(stable) {
		return ro.failf(phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"rollout: %q has no running stable version to canary against (active slot %q is not serving)\nHint: start the app or rebuild with --blue-green first",
			ro.AppName, state.ActiveSlot))
	}

	// 3. Strategy migration onto the blue-green topology. A rolling fleet is
	// consolidated: its first live replica is adopted as the blue (stable)
	// slot and keeps serving; the remaining replicas retire after the first
	// traffic switch, exactly like a rolling → blue-green migration.
	legacy, err := state.MigrateTo(ModeBlueGreen)
	if err != nil {
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "rollout: persist strategy migration"))
	}
	stableSlot := state.ActiveSlot
	if pre.mode == ModeRolling {
		adopted := cloneInstance(stable)
		adopted.Slot = SlotBlue
		if state.Slots == nil {
			state.Slots = map[string]*Instance{}
		}
		state.Slots[SlotBlue] = adopted
		state.Slots[SlotGreen] = &Instance{Slot: SlotGreen, Status: "stopped"}
		state.ActiveSlot = SlotBlue
		stable, stableSlot = adopted, SlotBlue
		legacy = withoutPID(legacy, adopted.PID)
		ro.storeState(state)
	}

	// 4. Recover leftovers. A Canary record still marked "running" means the
	// previous rollout was interrupted (crash, kill): its weighted routing
	// may still be live in the proxy, so the stable slot goes back to 100%
	// before anything else happens. Stale instances on non-active slots from
	// any aborted predecessor are reclaimed the same way blue-green does.
	if state.Canary != nil && state.Canary.Status == CanaryRunning {
		log.Stepf("recovering rollout of v%d interrupted at %d%% traffic", state.Canary.Version, state.Canary.TrafficPercent)
		// The restore must not inherit the caller's cancellation: even a
		// Ctrl-C'd rebuild has to leave the previous rollout converged on
		// stable before this deploy gives up.
		recCtx, recCancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultRolloutVerifyTimeout)
		if err := ro.restoreStableRouting(recCtx, state, stable); err != nil {
			log.Warnf("could not restore stable routing after the interrupted rollout: %v", err)
		}
		recCancel()
		state.Canary.Status = CanaryAborted
		state.Canary.Reason = "recovered: previous rollout was interrupted"
		state.Canary.UpdatedAt = time.Now()
		ro.storeState(state)
	}
	ReapStaleInstances(state)
	recoverStaleSlots(ctx, state, ro.AppName, grace, ro.inFlight(ro.AppName), log)
	ro.enrolled = ro.checkEnrolled(ctx)

	canarySlot := state.InactiveSlot()
	if state.Slots[canarySlot] == nil {
		state.Slots[canarySlot] = &Instance{Slot: canarySlot, Status: "stopped"}
	}

	// 5. Build the canary artifact.
	log.Stepf("%s for slot %s (stable %s keeps serving)", ro.Source.Describe(), canarySlot, stableVersionLabel(stable))
	ro.Telemetry.Building(ro.Source.Describe())
	binaryPath, envPath, err := ro.Source.Build(ctx)
	if err != nil {
		ro.undoMigration(state, pre)
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeBuildFailed, err, "rollout: prepare deploy artifact"))
	}
	log.Successf("artifact ready: %s", binaryPath)
	envOverlay, err := EnvOverlayFromSnapshot(envPath, ro.AppID)
	if err != nil {
		ro.undoMigration(state, pre)
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "rollout: load env snapshot"))
	}
	targetVer := targetVersionFromSource(ro.Source)
	ro.Telemetry.SetTargetVersion(targetVer)

	// 6. Start the canary on the inactive slot and gate it on the deploy-tier
	// health check — traffic is never routed to it before this passes.
	log.Stepf("starting canary instance on slot %s", canarySlot)
	proc, port, err := ro.Launcher(ctx, binaryPath, envOverlay)
	if err != nil {
		ro.undoMigration(state, pre)
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "rollout: start canary instance"))
	}
	canaryInst := &Instance{
		Slot:       canarySlot,
		PID:        proc.PID(),
		Port:       port,
		BinaryPath: binaryPath,
		EnvPath:    envPath,
		StartedAt:  time.Now(),
		Status:     "starting",
		Version:    targetVer,
	}
	state.Slots[canarySlot] = canaryInst
	if err := ro.persist(state); err != nil {
		stopHeldProcess(ctx, proc, grace)
		ro.undoMigration(state, pre)
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "rollout: persist canary instance state"))
	}
	ro.Telemetry.InstanceStarted(canarySlot, canaryInst.PID, port)

	tierCfg := ro.healthCfg()
	tier := selectDeployHealth(ctx, tierCfg, ro.AppName, hostPort(port), log, ro.Notifier)
	ro.Telemetry.SetHealthConfig(tierCfg, tier)
	ro.Telemetry.HealthCheckStarted(canarySlot, port)
	if err := health.WaitForHealthy(ctx, tier, tierCfg, hostPort(port), proc.PID(), deployResolver(proc)); err != nil {
		err = candidateHealthFailure(proc, binaryPath, port, tier, tierCfg, err)
		stopHeldProcess(ctx, proc, grace)
		canaryInst.Status = "failed"
		canaryInst.PID = 0
		ro.storeState(state)
		ro.undoMigration(state, pre)
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeHealthCheckFailed, err,
			"rollout aborted: canary unhealthy; stable instance untouched"))
	}
	canaryInst.Status = "running"
	if state.Health == nil {
		state.Health = &HealthSummary{}
	}
	state.Health.Tier = int(tier)
	state.Health.TierLabel = tier.String()
	state.Health.HealthyAt = time.Now()
	ro.Telemetry.InstanceHealthy(canarySlot, canaryInst.PID, port)

	// 7. Mark the rollout in flight BEFORE the first weighted switch: this
	// record is the crash marker the recovery above keys on.
	state.Canary = &CanaryState{
		Version:   targetVer,
		Strategy:  ro.Plan.Strategy,
		Slot:      canarySlot,
		Step:      0,
		Steps:     len(ro.Plan.Steps),
		Status:    CanaryRunning,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := ro.persist(state); err != nil {
		stopHeldProcess(ctx, proc, grace)
		canaryInst.Status = "failed"
		canaryInst.PID = 0
		ro.storeState(state)
		ro.undoMigration(state, pre)
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "rollout: persist in-flight rollout state"))
	}

	// abort terminates the rollout: stable routing is restored, the canary is
	// stopped and the failure is recorded. It is the single exit path for
	// every failure after the canary exists, which is what guarantees a failed
	// rollout always ends with the stable version at 100% of traffic.
	//
	// Cleanup runs on a context detached from the caller's cancellation: when
	// the operator aborts the rollout (Ctrl-C) the restore switch and the
	// drain must still complete instead of inheriting the cancellation.
	switched := false
	abort := func(reason error, status string, keepCanary bool) error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultRolloutVerifyTimeout+grace)
		defer cancel()
		if switched {
			if err := ro.restoreStableRouting(cleanupCtx, state, stable); err != nil {
				log.Errorf("could not restore stable routing after failed rollout: %v", err)
				// The proxy may still route part of the traffic to the canary.
				// Stopping it would drop those requests outright, so it stays
				// up serving and the failure is escalated.
				keepCanary = true
			}
		}
		if !keepCanary {
			stopHeldProcess(cleanupCtx, proc, grace)
			if ci := state.Slots[canarySlot]; ci != nil {
				ci.Status = "failed"
				ci.PID = 0
			}
		} else {
			log.Warnf("canary left running because the stable baseline cannot take the traffic back; manual intervention required")
		}
		if state.Canary == nil {
			state.Canary = &CanaryState{Version: targetVer, Strategy: ro.Plan.Strategy, Slot: canarySlot, Steps: len(ro.Plan.Steps)}
		}
		state.Canary.Status = status
		state.Canary.Reason = truncateReason(reason.Error())
		state.Canary.UpdatedAt = time.Now()
		ro.storeState(state)
		if !switched {
			ro.undoMigration(state, pre)
		}
		return ro.failf(reason)
	}

	// 8. Walk the steps.
	total := len(ro.Plan.Steps)
	for i, step := range ro.Plan.Steps {
		if err := ctx.Err(); err != nil {
			return abort(wrapDeployStep(err, fmt.Sprintf("rollout cancelled before step %d/%d", i+1, total)), CanaryAborted, false)
		}

		desc := fmt.Sprintf("step %d/%d: routing %d%% of traffic to the canary", i+1, total, step.TrafficPercent)
		if step.Duration > 0 {
			desc += fmt.Sprintf(" for %s", step.Duration)
		}
		ro.Telemetry.RolloutStepStarted(canarySlot, canaryInst.Port, desc)
		log.Stepf("%s", desc)

		// a. Switch traffic to this step's split. Weights travel with the
		// proxy targets; the stable slot stays the primary (and sole member
		// of the fallback path) for every share below 100%.
		var primary proxy.Target
		var backends []proxy.Target
		if step.TrafficPercent >= 100 {
			primary = proxy.Target{Host: hostPort(canaryInst.Port), Label: canarySlot}
		} else {
			primary = proxy.Target{Host: hostPort(stable.Port), Label: stableSlot, Weight: 100 - step.TrafficPercent}
			backends = []proxy.Target{{Host: hostPort(canaryInst.Port), Label: canarySlot, Weight: step.TrafficPercent}}
		}
		ro.Telemetry.ProxySwitching(canarySlot, canaryInst.Port,
			fmt.Sprintf("routing %d%% of public port %d traffic to the canary", step.TrafficPercent, ro.PublicPort))
		if err := ro.switchTraffic(ctx, primary, backends...); err != nil {
			return abort(phelixerr.Wrapf(phelixerr.CodeProxy, err,
				"rollout failed at step %d/%d: proxy traffic switch failed", i+1, total), CanaryFailed, false)
		}
		switched = true
		ro.Telemetry.ProxySwitched(canarySlot, canaryInst.Port,
			fmt.Sprintf("public port %d now splits traffic %d%%/%d%%", ro.PublicPort, 100-step.TrafficPercent, step.TrafficPercent))
		ro.Telemetry.SetProxy(ro.PublicPort, primary.Label, primaryPort(primary), upstreamHosts(primary, backends))
		if step.TrafficPercent >= 100 {
			log.Successf("traffic switched fully to slot %s (zero downtime)", canarySlot)
		} else {
			stableLine, canaryLine := trafficSplitLines(stableVersionLabel(stable), 100-step.TrafficPercent, versionLabel(targetVer), step.TrafficPercent)
			log.Infof("%s", stableLine)
			log.Infof("%s", canaryLine)
		}
		// b. Persist the step progress before observing it, so a crash here
		// still points recovery at the right traffic share.
		state.Canary.Step = i
		state.Canary.TrafficPercent = step.TrafficPercent
		state.Canary.UpdatedAt = time.Now()
		if err := ro.persist(state); err != nil {
			return abort(phelixerr.Wrapf(phelixerr.CodeFilesystem, err,
				"rollout failed at step %d/%d: persist step state", i+1, total), CanaryFailed, false)
		}

		// c. Verify the canary under this traffic share.
		verify := ro.Verify
		if verify == nil {
			verify = ro.verifyStep
		}
		if err := verify(ctx, StepContext{
			Step:           i,
			TotalSteps:     total,
			TrafficPercent: step.TrafficPercent,
			Duration:       step.Duration,
			CanarySlot:     canarySlot,
			CanaryPort:     canaryInst.Port,
			CanaryPID:      canaryInst.PID,
			CanaryVersion:  targetVer,
			StableSlot:     stableSlot,
			StablePort:     stable.Port,
			StablePID:      stable.PID,
			StableVersion:  stable.Version,
			Tier:           tier,
			TierCfg:        tierCfg,
		}); err != nil {
			status := CanaryFailed
			reason := err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = CanaryAborted
				// A bare "context canceled" tells the operator nothing about
				// where the rollout stopped.
				reason = wrapDeployStep(err, fmt.Sprintf("rollout cancelled while verifying step %d/%d", i+1, total))
			}
			return abort(reason, status, errors.Is(err, ErrStableBaselineUnhealthy))
		}
		ro.Telemetry.RolloutStepVerified(canarySlot, canaryInst.Port,
			fmt.Sprintf("step %d/%d verified at %d%% traffic", i+1, total, step.TrafficPercent))

		// d. After the first successful switch the previous strategy's fleet
		// is out of the serving path and may retire.
		if i == 0 && len(legacy) > 0 {
			stopRetiredInstances(ctx, legacy, grace, ro.inFlight(ro.AppName), log)
			legacy = nil
		}
	}

	// 9. Final step verified with the canary at 100%: promote durably. The
	// ordering mirrors blue-green's commit: persist the post-switch serving
	// state, promote the version, and only then drain the old instance.
	oldSlot := state.ActiveSlot
	oldVersion := state.ActiveVersion
	oldInst := state.Slots[oldSlot]
	state.ActiveSlot = canarySlot
	state.ActiveVersion = targetVer
	state.Canary.Status = CanaryPromoted
	state.Canary.UpdatedAt = time.Now()
	if err := ro.persist(state); err != nil {
		// Compensate: traffic back on stable, canary stopped, pre-promotion
		// state restored. If the compensation itself fails, the canary may
		// still be serving — keep it alive and record the best-known truth.
		state.ActiveSlot = oldSlot
		state.ActiveVersion = oldVersion
		compCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultRolloutVerifyTimeout)
		defer cancel()
		if compErr := ro.restoreStableRouting(compCtx, state, stable); compErr != nil {
			state.ActiveSlot = canarySlot
			state.ActiveVersion = targetVer
			_ = ro.persist(state)
			return ro.failf(phelixerr.Wrapf(phelixerr.CodeFilesystem, err,
				"rollout: persist promotion failed and stable compensation failed: %v", compErr))
		}
		stopHeldProcess(compCtx, proc, grace)
		canaryInst.Status = "failed"
		canaryInst.PID = 0
		state.Canary.Status = CanaryFailed
		ro.storeState(state)
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeFilesystem, err,
			"rollout: persist promotion failed; traffic restored to stable"))
	}
	if targetVer > 0 {
		if err := PromoteVersion(ro.AppName, targetVer, string(ModeBlueGreen)); err != nil {
			state.ActiveSlot = oldSlot
			state.ActiveVersion = oldVersion
			compCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultRolloutVerifyTimeout)
			defer cancel()
			var compErr error
			if oldInst != nil && oldInst.Port > 0 {
				compErr = ro.restoreStableRouting(compCtx, state, stable)
			}
			if compErr == nil {
				stopHeldProcess(compCtx, proc, grace)
				canaryInst.Status = "failed"
				canaryInst.PID = 0
				state.Canary.Status = CanaryFailed
				if persistErr := ro.persist(state); persistErr != nil {
					return ro.failf(phelixerr.Wrapf(phelixerr.CodeFilesystem, err,
						"rollout: promote failed; traffic restored but state restore failed: %v", persistErr))
				}
				return ro.failf(err)
			}
			// Traffic may still reach the canary; keep it alive and persist
			// the best-known serving truth rather than causing an outage.
			state.ActiveSlot = canarySlot
			state.ActiveVersion = targetVer
			_ = ro.persist(state)
			return ro.failf(phelixerr.Wrapf(phelixerr.CodeProxy, compErr,
				"rollout: promote failed and stable compensation failed: %v", err))
		}
	}
	ro.Telemetry.PromoteCurrentVersion()
	ro.Telemetry.SetProxy(ro.PublicPort, canarySlot, canaryInst.Port, []string{hostPort(canaryInst.Port)})

	// 10. The promotion is durable; the old stable instance can drain.
	if oldInst != nil && oldInst.PID > 0 {
		log.Stepf("draining previous stable slot %s (pid %d, grace %s)", oldSlot, oldInst.PID, grace)
		ro.Telemetry.InstanceDraining(oldSlot, oldInst.PID)
		stoppedPID := oldInst.PID
		report := stopByPID(ctx, oldInst.PID, grace, ro.inFlight(ro.AppName), oldInst.BinaryPath)
		if report.ForceKilled {
			log.Warnf("old slot %s did not exit within grace; SIGKILL applied", oldSlot)
		} else {
			log.Successf("old slot %s drained and exited in %s", oldSlot, report.Elapsed.Round(time.Millisecond))
		}
		oldInst.Status = "stopped"
		oldInst.PID = 0
		ro.Telemetry.InstanceStopped(oldSlot, stoppedPID, drainOutcome(report))
	}
	if err := ro.persist(state); err != nil {
		return ro.failf(phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "rollout: persist drained old-slot state"))
	}

	ver := stableVersionLabel(canaryInst)
	log.Successf("%s rollout of %s complete: %s serves 100%% of traffic on slot %s",
		ro.Plan.Strategy, ro.AppName, ver, canarySlot)
	ro.Telemetry.Completed(fmt.Sprintf("%s promoted to 100%% of traffic on slot %s", ver, canarySlot))
	return nil
}

// --- traffic switching ------------------------------------------------------

// switchTraffic points the proxy at this step's split, enrolling the app on
// the first call (mirroring the blue-green Add/Switch distinction).
func (ro *Rollout) switchTraffic(ctx context.Context, primary proxy.Target, backends ...proxy.Target) error {
	if ro.ProxyClient == nil {
		return phelixerr.New(phelixerr.CodeProxy, "rollout: no proxy client")
	}
	if !ro.enrolled {
		// The public port may still be owned by a classic instance from
		// before the app came under the proxy; hand it off now — the canary
		// is already healthy, so the window is only the handoff itself.
		if ro.PortHandoff != nil && ro.PublicPort > 0 && !phelixport.IsAvailable(ro.PublicPort) {
			log := ro.logger()
			log.Stepf("public port %d is owned by a classic instance; handing it to the proxy", ro.PublicPort)
			if err := ro.PortHandoff(ctx, ro.AppName, ro.PublicPort); err != nil {
				return phelixerr.Wrapf(phelixerr.CodeProxy, err, "classic handoff failed")
			}
			if !waitForPortRelease(ro.PublicPort, 5*time.Second) {
				return phelixerr.Newf(phelixerr.CodePortUnavailable,
					"public port %d still bound after stopping the classic instance", ro.PublicPort)
			}
		}
		if err := ro.ProxyClient.Add(ctx, ro.AppName, ro.PublicPort, primary, backends...); err != nil {
			return err
		}
		ro.enrolled = true
		return nil
	}
	return ro.ProxyClient.Switch(ctx, ro.AppName, primary, backends...)
}

// restoreStableRouting routes 100% of traffic back to the stable slot. It is
// the compensation path for every failed or interrupted rollout; the caller is
// responsible for only invoking it when traffic was actually moved.
func (ro *Rollout) restoreStableRouting(ctx context.Context, state *DeployState, stable *Instance) error {
	if ro.ProxyClient == nil {
		return phelixerr.New(phelixerr.CodeProxy, "rollout: no proxy client")
	}
	if stable == nil || stable.Port <= 0 {
		return phelixerr.New(phelixerr.CodeProxy, "rollout: no stable target to restore routing to")
	}
	ro.logger().Stepf("restoring stable version to 100%% of traffic")
	return ro.ProxyClient.Switch(ctx, ro.AppName, proxy.Target{Host: hostPort(stable.Port), Label: stable.Slot})
}

// checkEnrolled asks the daemon whether this app is registered so the first
// traffic switch knows to Add before any Switch. Unknown/unreachable daemon
// states conservatively report "not enrolled" — the Add then fails with a
// clear error instead of switches silently routing nothing.
func (ro *Rollout) checkEnrolled(ctx context.Context) bool {
	if ro.ProxyClient == nil {
		return false
	}
	st, err := ro.ProxyClient.Status(ctx, ro.AppName)
	return err == nil && len(st) > 0
}

// --- helpers ----------------------------------------------------------------

func (ro *Rollout) logger() Logger {
	if ro.Logger != nil {
		return ro.Logger
	}
	return &nopLogger{}
}

func (ro *Rollout) healthCfg() *health.DeployTierConfig {
	if ro.HealthProvider != nil {
		return ro.HealthProvider(ro.AppID)
	}
	return nil
}

func (ro *Rollout) inFlight(appName string) int64 {
	if ro.InFlight == nil {
		return 0
	}
	return ro.InFlight(appName)
}

func (ro *Rollout) persist(state *DeployState) error {
	if ro.stateStore != nil {
		return ro.stateStore(state)
	}
	return Store(state)
}

// storeState persists deploy state, surfacing persistence failures through the
// Logger instead of discarding them silently (same contract as blue-green).
func (ro *Rollout) storeState(state *DeployState) {
	if err := ro.persist(state); err != nil {
		ro.logger().Warnf("failed to persist deploy state for %s: %v", ro.AppName, err)
	}
}

// failf logs err and returns it unchanged, so Deploy's callers see a single
// structured error and the telemetry's terminal event can never disagree with
// what Deploy returned.
func (ro *Rollout) failf(err error) error {
	if err != nil {
		ro.logger().Errorf("%v", err)
		reportDeployFailure(ro.Telemetry, err)
	}
	return err
}

// undoMigration reverts a strategy migration whose rollout failed before it
// moved any traffic, so deploy.json again describes the deployment that is
// still actually serving (e.g. the rolling fleet that owns the proxy).
func (ro *Rollout) undoMigration(state *DeployState, pre modeSnapshot) {
	if pre.mode == "" || pre.mode == ModeBlueGreen {
		return
	}
	state.restoreMode(pre)
	if err := ro.persist(state); err != nil {
		ro.logger().Warnf("failed to persist strategy migration rollback: %v", err)
	}
}

// firstLiveReplica returns the lowest-index replica whose process is alive and
// verifiable, or nil when the fleet has none.
func firstLiveReplica(state *DeployState) *Instance {
	for _, k := range sortedReplicaKeys(state.Replicas) {
		if inst := state.Replicas[k]; inst != nil && inst.PID > 0 && InstanceAlive(inst) {
			return inst
		}
	}
	return nil
}

// withoutPID filters keepPID out of the retired-instance list (used to keep an
// adopted replica alive when a rolling fleet is consolidated).
func withoutPID(in []*Instance, keepPID int) []*Instance {
	out := make([]*Instance, 0, len(in))
	for _, inst := range in {
		if inst == nil || inst.PID == keepPID {
			continue
		}
		out = append(out, inst)
	}
	return out
}

func primaryPort(t proxy.Target) int {
	_, port, err := health.MustParseHostPort(t.Host)
	if err != nil {
		return 0
	}
	return port
}

// stableVersionLabel renders the stable version for progress output; an
// unknown version still gets a name instead of an empty string.
func stableVersionLabel(inst *Instance) string {
	if inst == nil || inst.Version <= 0 {
		return "stable"
	}
	return versionLabel(inst.Version)
}

// trafficSplitLines renders the stable/canary traffic split as two aligned
// bar lines for the CLI progress output:
//
//	v12  ███████████████████  95%
//	v13  █                    5%
//
// Two separate strings (not one multi-line blob) so each logger indents its
// own line: a single string would only indent the first line.
func trafficSplitLines(stableLabel string, stablePct int, canaryLabel string, canaryPct int) (string, string) {
	const width = 20
	bar := func(pct int) string {
		n := (pct*width + 50) / 100
		if n < 0 {
			n = 0
		}
		if n > width {
			n = width
		}
		return strings.Repeat("█", n) + strings.Repeat(" ", width-n)
	}
	line := func(label string, pct int) string {
		return fmt.Sprintf("%-4s %s %3d%%", label, bar(pct), pct)
	}
	return line(stableLabel, stablePct), line(canaryLabel, canaryPct)
}
