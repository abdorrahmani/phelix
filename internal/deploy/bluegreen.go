package deploy

import (
	"context"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// DefaultGracePeriod is the time allowed for in-flight requests to drain on the
// old instance before it is force-killed. Configurable per-app.
const DefaultGracePeriod = 30 * time.Second

// Logger is the minimal logging contract the deploy flow needs. Implementations
// forward to fmt.Println / color / the gRPC monitor stream as appropriate.
type Logger interface {
	Stepf(format string, args ...any)    // normal progress ("→ ...")
	Infof(format string, args ...any)    // informational
	Warnf(format string, args ...any)    // warnings (Tier 2/3 fallback)
	Successf(format string, args ...any) // success ("✓ ...")
	Errorf(format string, args ...any)   // errors ("✗ ...")
}

// Notifier sends a message to an out-of-band channel (the monitor "panel").
// Deploy uses it for the Tier 2/3 fallback warning so the operator is alerted
// both in the terminal (via Logger.Warnf) and in the panel.
type Notifier interface {
	Notify(ctx context.Context, message string) error
}

// HealthConfigProvider returns the deploy-tier health config for an app, or nil
// when none is configured (which triggers auto tier selection). The default
// implementation reads health.ConfigManager by app ID.
type HealthConfigProvider func(appID string) *health.DeployTierConfig

// ProxyClient is the subset of proxy.Client the deploy flow uses. Declared
// locally so tests can inject a fake.
type ProxyClient interface {
	Ping(ctx context.Context) error
	Add(ctx context.Context, appName string, publicPort int, primary proxy.Target, backends ...proxy.Target) error
	Switch(ctx context.Context, appName string, primary proxy.Target, backends ...proxy.Target) error
	Remove(ctx context.Context, appName string) error
	Status(ctx context.Context, appName string) ([]proxy.AppStatus, error)
}

// Builder runs the build for the new instance. It returns the absolute path to
// the freshly built binary. The default implementation delegates to the
// existing internal/builder package.
type Builder func(ctx context.Context, appID, appName string, extraArgs []string) (binaryPath string, err error)

// InFlightProvider returns the in-flight request count for an app's proxy at
// shutdown time, for observability. May return 0 when unknown.
type InFlightProvider func(appName string) int64

// BlueGreen holds the dependencies and configuration for a blue-green deploy.
type BlueGreen struct {
	AppName    string
	AppID      string
	PublicPort int
	ExtraArgs  []string

	Builder        Builder // deprecated: use Source; kept for tests via BuilderSource
	Source         BuildSource
	Launcher       InstanceLauncher
	ProxyClient    ProxyClient
	HealthProvider HealthConfigProvider
	Logger         Logger
	Notifier       Notifier
	// InFlight is optional; nil reports 0 in-flight at shutdown.
	InFlight InFlightProvider
	// GracePeriod overrides DefaultGracePeriod when > 0.
	GracePeriod time.Duration
	// PortHandoff, when set, is invoked right before the first proxy
	// enrolment if something still owns the public port — almost always a
	// classic instance from before the app migrated to blue-green. The CLI
	// wires this to "gracefully stop the app's own classic process". It runs
	// only AFTER the replacement instance is healthy, so the downtime window
	// is the seconds between the classic process exiting and the proxy
	// binding the port. Returning an error aborts the deploy with the
	// candidate killed and the classic instance untouched.
	PortHandoff func(ctx context.Context, appName string, publicPort int) error
}

// Deploy performs one blue-green deployment. The sequence mirrors the project
// spec exactly:
//
//  1. Load state; pick the inactive slot.
//  2. Ensure the proxy daemon is reachable (else error clearly).
//  3. Build the new binary.
//  4. Start the instance on the inactive slot's internal port.
//  5. Select the health tier; warn (terminal + panel) if Tier 2/3.
//  6. Wait for healthy within the timeout. On failure: kill the new instance,
//     leave the active one untouched, return a non-nil error.
//  7. Atomically switch the proxy's target to the new instance.
//  8. Gracefully stop the old instance.
//  9. Persist state.
//
// On the very first deploy there is no active instance: steps 2 and 7 enrol
// the app with the proxy, and step 8 is skipped.
func (bg *BlueGreen) Deploy(ctx context.Context) (errRet error) {
	log := bg.logger()
	grace := bg.GracePeriod
	if grace <= 0 {
		grace = DefaultGracePeriod
	}

	src := bg.Source
	if src == nil && bg.Builder != nil {
		src = BuilderSource(bg.Builder)
	}
	if src == nil {
		return bg.failf(phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: no BuildSource or Builder configured"))
	}

	// 1. Load / initialise state.
	state, err := LoadOrInit(bg.AppName, ModeBlueGreen, bg.PublicPort)
	if err != nil {
		return bg.failf(phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "failed to load deploy state"))
	}
	state.AppID = bg.AppID

	// Strategy migration (e.g. rolling → blue-green): switch the recorded mode
	// up front and keep the previous strategy's instances — they keep serving
	// until the proxy switch succeeds, then they are drained. If the deploy
	// fails before the switch, the migration is undone via the deferred hook
	// below so the state again describes the deployment that is serving.
	pre := captureMode(state)
	if pre.mode != "" && pre.mode != ModeBlueGreen {
		log.Stepf("strategy migration: %s → blue-green (previous strategy's instances retire after the traffic switch)", pre.mode)
	}
	legacy, err := state.MigrateTo(ModeBlueGreen)
	if err != nil {
		return bg.failf(phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "persist strategy migration to blue-green"))
	}
	switched := false
	defer func() {
		if errRet != nil && !switched {
			if pre.mode != "" && pre.mode != ModeBlueGreen {
				state.restoreMode(pre)
				_ = Store(state)
			} else if pre.mode == "" {
				// First-ever blue-green attempt: remove the half-created
				// deploy state so reconciliation keeps trusting the classic
				// app record that still serves reality.
				_ = RemoveState(bg.AppName)
			}
		}
	}()
	inactive := state.InactiveSlot()
	active := state.ActiveSlot

	log.Stepf("blue-green deploy for %s: active=%q, deploying slot=%q", bg.AppName, active, inactive)

	// 1b. Recover leftovers from a previously interrupted deploy before we
	// touch anything else: stale non-active instances would otherwise be
	// orphaned when this deploy overwrites their slot records.
	bg.recoverStaleSlots(ctx, state, grace)
	if state.Slots[inactive] == nil {
		state.Slots[inactive] = &Instance{Slot: inactive, Status: "stopped"}
	}
	bg.storeState(state)

	// 2. Proxy daemon must be reachable; enrol if this is the first deploy.
	proxyOK := bg.ProxyClient != nil && bg.pingProxy(ctx) == nil
	// The proxy daemon — not the persisted slot record — is the source of
	// truth for who currently serves traffic. A classic build/rebuild stops
	// the proxy-managed instance and starts one that binds the public port
	// itself, leaving deploy.json's active slot stale; a daemon reset can do
	// the same. Switching against an unenrolled app can only fail, so an
	// unenrolled app always takes the enrol (Add) path.
	proxyEnrolled := bg.proxyEnrolled(ctx)

	// 3. Prepare binary + paired env snapshot (fresh compile or rollback target).
	log.Stepf("%s for slot %s", src.Describe(), inactive)
	binaryPath, envPath, err := src.Build(ctx)
	if err != nil {
		return bg.failf(phelixerr.Wrapf(phelixerr.CodeBuildFailed, err, "prepare deploy artifact failed"))
	}
	log.Successf("artifact ready: %s", binaryPath)
	envOverlay, err := EnvOverlayFromSnapshot(envPath, bg.AppID)
	if err != nil {
		return bg.failf(phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "load env snapshot"))
	}

	// 4. Start the new instance on a free internal port.
	log.Stepf("starting new instance on slot %s", inactive)
	proc, port, err := bg.Launcher(ctx, binaryPath, envOverlay)
	if err != nil {
		return bg.failf(phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "start instance failed"))
	}
	targetVer := targetVersionFromSource(src)
	newInst := &Instance{
		Slot:       inactive,
		PID:        proc.PID(),
		Port:       port,
		BinaryPath: binaryPath,
		EnvPath:    envPath,
		StartedAt:  time.Now(),
		Status:     "starting",
		Version:    targetVer,
	}
	if state.Slots == nil {
		state.Slots = map[string]*Instance{SlotBlue: {Slot: SlotBlue}, SlotGreen: {Slot: SlotGreen}}
	}
	// The instance is NOT "running" yet — it has merely been spawned. Keeping
	// the persisted status truthful ("starting") matters for recovery paths
	// that distinguish failed spawns from live-but-unproven instances.
	newInst.Status = "starting"
	state.Slots[inactive] = newInst
	bg.storeState(state) // persist intermediate state so a crash here is observable

	// 5. Select the health tier for the new instance (shared with rolling).
	tierCfg := bg.healthCfg()
	tier := selectDeployHealth(ctx, tierCfg, bg.AppName, hostPort(port), log, bg.Notifier)

	// 6. Wait for healthy. On failure, abort and leave the active instance
	//    untouched.
	if err := health.WaitForHealthy(ctx, tier, tierCfg, hostPort(port), proc.PID(), nil); err != nil {
		err = candidateHealthFailure(binaryPath, port, err)
		log.Errorf("new instance on slot %s failed health check: %v", inactive, err)
		// Kill the failed instance; do NOT touch the active one.
		_, _ = GracefulStop(ctx, proc, grace, bg.inFlight(bg.AppName))
		newInst.Status = "failed"
		newInst.PID = 0
		bg.storeState(state)
		return bg.failf(phelixerr.Wrapf(
			phelixerr.CodeHealthCheckFailed,
			err,
			"deploy aborted: new instance unhealthy; active instance untouched",
		))
	}
	newInst.Status = "running"
	if state.Health == nil {
		state.Health = &HealthSummary{}
	}
	state.Health.Tier = int(tier)
	state.Health.TierLabel = tier.String()
	state.Health.HealthyAt = time.Now()

	// 7. Atomically switch the proxy to the new instance.
	primary := proxy.Target{Host: hostPort(port), Label: inactive}
	if active == "" || !proxyEnrolled {
		// First deploy: enrol the app. If the proxy wasn't reachable earlier,
		// this is a hard error — we cannot offer zero-downtime without it.
		if bg.ProxyClient == nil {
			_, _ = GracefulStop(ctx, proc, grace, 0)
			newInst.Status = "failed"
			newInst.PID = 0
			bg.storeState(state)
			return bg.failf(phelixerr.New(phelixerr.CodeProxy, "deploy aborted: no proxy client (is 'phelix proxy' running?)"))
		}
		if !proxyOK {
			_, _ = GracefulStop(ctx, proc, grace, 0)
			newInst.Status = "failed"
			newInst.PID = 0
			bg.storeState(state)
			return bg.failf(phelixerr.New(phelixerr.CodeConnection, "deploy aborted: proxy daemon unreachable (is 'phelix proxy' running?)"))
		}
		// Classic → blue-green migration: stop the classic instance that owns
		// the public port so the proxy can bind it. Runs after the candidate
		// is healthy (pre-warmed), so the interruption is a brief port
		// handoff rather than a stop-then-deploy round trip. Skipped when the
		// app is already enrolled: the port then belongs to the proxy itself
		// (e.g. a rolling → blue-green migration), not to a classic process.
		if !proxyEnrolled {
			if err := bg.handOffPublicPort(ctx, proc, log); err != nil {
				_, _ = GracefulStop(ctx, proc, grace, 0)
				newInst.Status = "failed"
				newInst.PID = 0
				bg.storeState(state)
				return bg.failf(err)
			}
		}
		if err := bg.ProxyClient.Add(ctx, bg.AppName, bg.PublicPort, primary); err != nil {
			_, _ = GracefulStop(ctx, proc, grace, 0)
			newInst.Status = "failed"
			bg.storeState(state)
			// First deploy of an app that was started via the classic
			// stop→build→start flow: the old instance still binds the public
			// port itself, so the proxy cannot take it over yet. One
			// deliberate stop migrates the app under the proxy; every deploy
			// after that is zero-downtime (Switch, not Add).
			return bg.failf(phelixerr.Wrapf(phelixerr.CodeProxy, err,
				"enrol app with proxy failed (if %q is currently running outside the proxy, stop it once with 'phelix stop %s' — later deploys switch with zero downtime)",
				bg.AppName, bg.AppName))
		}
		log.Successf("enrolled %s with proxy on public port %d -> slot %s", bg.AppName, bg.PublicPort, inactive)
		// The stale slot record no longer describes reality (the app it
		// pointed at is not what the proxy serves); normalize it.
		active = ""
		switched = true
	} else {
		if bg.ProxyClient == nil {
			// We must not leak the healthy instance we just started. Traffic
			// never switched, so stopping it keeps the active slot untouched.
			log.Errorf("proxy switch aborted: no proxy client")
			_, _ = GracefulStop(ctx, proc, grace, bg.inFlight(bg.AppName))
			newInst.Status = "failed"
			newInst.PID = 0
			bg.storeState(state)
			return bg.failf(phelixerr.New(phelixerr.CodeProxy, "deploy aborted: no proxy client for switch"))
		}
		if err := bg.ProxyClient.Switch(ctx, bg.AppName, primary); err != nil {
			// The new instance is healthy and running; we switch failure means
			// traffic is still on the old instance. Kill the new one and abort.
			log.Errorf("proxy switch failed: %v", err)
			_, _ = GracefulStop(ctx, proc, grace, bg.inFlight(bg.AppName))
			newInst.Status = "failed"
			bg.storeState(state)
			return bg.failf(phelixerr.Wrapf(
				phelixerr.CodeProxy,
				err,
				"deploy aborted: proxy switch failed; active instance untouched",
			))
		}
		log.Successf("traffic switched to slot %s (zero downtime)", inactive)
		switched = true
	}

	// 8. Promote the new slot and gracefully stop the old instance. Version and
	// "current" symlink are updated only after the proxy switch succeeds — same
	// rule as forward deploy (never point current at an unhealthy instance).
	oldActive := active
	state.ActiveSlot = inactive
	// Retired instances from a previous strategy (rolling → blue-green): the
	// proxy no longer routes to them, so drain and stop them here alongside
	// the previous active slot.
	stopRetiredInstances(ctx, legacy, grace, bg.inFlight(bg.AppName), log)
	legacy = nil
	if targetVer > 0 {
		state.ActiveVersion = targetVer
		if err := PromoteVersion(bg.AppName, targetVer, string(ModeBlueGreen)); err != nil {
			log.Warnf("failed to promote version v%d: %v (traffic already routed)", targetVer, err)
		}
	}
	if err := Store(state); err != nil {
		log.Warnf("failed to persist state after switch: %v (traffic already routed)", err)
	}

	if oldActive != "" {
		if old := state.Slots[oldActive]; old != nil && old.PID > 0 {
			log.Stepf("gracefully stopping old slot %s (pid %d, grace %s)", oldActive, old.PID, grace)
			// We no longer hold the *os.Process for the previous instance (it
			// was started by a prior invocation), so stop it by PID. The
			// expected executable path guards against PID recycling.
			report := stopByPID(ctx, old.PID, grace, bg.inFlight(bg.AppName), old.BinaryPath)
			if report.ForceKilled {
				log.Warnf("old slot %s did not exit within grace; SIGKILL applied (in-flight: %d)", oldActive, report.InFlight)
			} else {
				log.Successf("old slot %s drained and exited in %s", oldActive, report.Elapsed.Round(time.Millisecond))
			}
			old.Status = "stopped"
			old.PID = 0
		}
	}

	bg.storeState(state)
	log.Successf("blue-green deploy of %s complete: active slot %s (pid %d, port %d)",
		bg.AppName, inactive, newInst.PID, newInst.Port)
	return nil
}

// --- helpers ---------------------------------------------------------------

func (bg *BlueGreen) logger() Logger {
	if bg.Logger != nil {
		return bg.Logger
	}
	return &nopLogger{}
}

func (bg *BlueGreen) healthCfg() *health.DeployTierConfig {
	if bg.HealthProvider != nil {
		return bg.HealthProvider(bg.AppID)
	}
	return nil
}

func (bg *BlueGreen) pingProxy(ctx context.Context) error {
	if bg.ProxyClient == nil {
		return phelixerr.New(phelixerr.CodeProxy, "no proxy client")
	}
	return bg.ProxyClient.Ping(ctx)
}

// proxyEnrolled asks the daemon whether the app is currently registered.
// Unreachable/unknown daemon states conservatively report "not enrolled":
// the subsequent Add then fails with a clear error instead of a Switch
// silently routing nothing.
func (bg *BlueGreen) proxyEnrolled(ctx context.Context) bool {
	if bg.ProxyClient == nil {
		return false
	}
	st, err := bg.ProxyClient.Status(ctx, bg.AppName)
	return err == nil && len(st) > 0
}

func (bg *BlueGreen) inFlight(appName string) int64 {
	if bg.InFlight == nil {
		return 0
	}
	return bg.InFlight(appName)
}

// storeState persists deploy state, surfacing persistence failures through the
// Logger instead of discarding them silently: an unpersisted slot/pid update
// is exactly how stale-instance records appear after a crash.
func (bg *BlueGreen) storeState(state *DeployState) {
	if err := Store(state); err != nil {
		bg.logger().Warnf("failed to persist deploy state for %s: %v", bg.AppName, err)
	}
}

// recoverStaleSlots reclaims resources left behind by a previously crashed or
// interrupted deploy: any non-active slot whose recorded process is still
// alive cannot be receiving traffic (the active slot owns it), so it is a
// leftover that would otherwise run forever while its record gets overwritten
// by the next deploy. Processes are verified by executable path before being
// signalled so PID recycling can never make Phelix kill an unrelated victim.
func (bg *BlueGreen) recoverStaleSlots(ctx context.Context, state *DeployState, grace time.Duration) {
	for name, inst := range state.Slots {
		if name == state.ActiveSlot || inst == nil || inst.PID <= 0 {
			continue
		}
		proc := findVerifiedProcess(inst.PID, inst.BinaryPath)
		if proc == nil {
			if inst.Status == "running" || inst.Status == "starting" {
				inst.Status = "stopped"
				inst.PID = 0
			}
			continue // genuinely gone — nothing to reclaim
		}
		bg.logger().Stepf("recovering stale %s instance from previous deploy (slot %s, pid %d)", bg.AppName, name, inst.PID)
		report := stopByPID(ctx, inst.PID, grace, bg.inFlight(bg.AppName), inst.BinaryPath)
		if report.ForceKilled {
			bg.logger().Warnf("stale slot %s ignored SIGTERM; SIGKILL applied", name)
		}
		inst.Status = "stopped"
		inst.PID = 0
	}
}

// failf logs err and returns it unchanged so Deploy's callers see a single
// structured error. It is the one place blue-green errors reach the Logger.
func (bg *BlueGreen) failf(err error) error {
	if err != nil {
		bg.logger().Errorf("%v", err)
	}
	return err
}

// nopLogger is the default no-op logger used when none is provided.
type nopLogger struct{}

func (nopLogger) Stepf(string, ...any)    {}
func (nopLogger) Infof(string, ...any)    {}
func (nopLogger) Warnf(string, ...any)    {}
func (nopLogger) Successf(string, ...any) {}
func (nopLogger) Errorf(string, ...any)   {}

// DefaultHealthProvider returns a HealthConfigProvider that reads the deploy
// tier config from the health package's ConfigManager by app ID.
//
// The manager must be initialized here: the CLI deploy path (`rebuild
// --blue-green` / `--replicas`) never touches the health commands, so the
// provider itself performs the (idempotent) initialization. The previous
// version called GetConfigManager unguarded-then-recovered, which silently
// swallowed the "not initialized" panic and returned nil — deploys then
// claimed "No health endpoint configured" for apps that had one, while
// `phelix health list/status` (which do initialize the manager) saw it fine.
func DefaultHealthProvider() HealthConfigProvider {
	return func(appID string) *health.DeployTierConfig {
		cm, err := health.InitConfigManager()
		if err != nil {
			return nil
		}
		return cm.GetConfig(appID).EffectiveDeployTier()
	}
}
