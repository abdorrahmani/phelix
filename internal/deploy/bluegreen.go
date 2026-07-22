package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// DefaultGracePeriod is the time allowed for in-flight requests to drain on the
// old instance before it is force-killed. Configurable per-app.
const DefaultGracePeriod = 30 * time.Second

// Logger is the minimal logging contract the deploy flow needs. Implementations
// forward to fmt.Println / color / the monitor websocket as appropriate.
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

	Builder        Builder
	Launcher       InstanceLauncher
	ProxyClient    ProxyClient
	HealthProvider HealthConfigProvider
	Logger         Logger
	Notifier       Notifier
	// InFlight is optional; nil reports 0 in-flight at shutdown.
	InFlight InFlightProvider
	// GracePeriod overrides DefaultGracePeriod when > 0.
	GracePeriod time.Duration
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

	// 1. Load / initialise state.
	state, err := LoadOrInit(bg.AppName, ModeBlueGreen, bg.PublicPort)
	if err != nil {
		return bg.failf("failed to load deploy state: %w", err)
	}
	state.AppID = bg.AppID
	inactive := state.InactiveSlot()
	active := state.ActiveSlot

	log.Stepf("blue-green deploy for %s: active=%q, deploying slot=%q", bg.AppName, active, inactive)

	// 2. Proxy daemon must be reachable; enrol if this is the first deploy.
	proxyOK := bg.ProxyClient != nil && bg.pingProxy(ctx) == nil

	// 3. Build the new binary.
	log.Stepf("building new binary for slot %s", inactive)
	binaryPath, err := bg.Builder(ctx, bg.AppID, bg.AppName, bg.ExtraArgs)
	if err != nil {
		return bg.failf("build failed: %w", err)
	}
	log.Successf("build complete: %s", binaryPath)

	// 4. Start the new instance on a free internal port.
	log.Stepf("starting new instance on slot %s", inactive)
	proc, port, err := bg.Launcher(ctx, binaryPath, nil)
	if err != nil {
		return bg.failf("start instance failed: %w", err)
	}
	newInst := &Instance{
		Slot:       inactive,
		PID:        proc.PID(),
		Port:       port,
		BinaryPath: binaryPath,
		StartedAt:  time.Now(),
		Status:     "starting",
	}
	if state.Slots == nil {
		state.Slots = map[string]*Instance{SlotBlue: {Slot: SlotBlue}, SlotGreen: {Slot: SlotGreen}}
	}
	state.Slots[inactive] = newInst
	newInst.Status = "running"
	_ = Store(state) // persist intermediate state so a crash here is observable

	// 5. Select the health tier for the new instance.
	tierCfg := bg.healthCfg()
	tier := health.SelectTier(tierCfg, hostPort(port), nil)
	log.Infof("health tier selected: %s", tier)

	if tier != health.Tier1HTTPPath {
		msg := fmt.Sprintf("⚠ No health endpoint configured for %s — using %s.\n"+
			"  Add one with: phelix health set %s --path /your-health-path",
			bg.AppName, tier, bg.AppName)
		log.Warnf("%s", msg)
		if bg.Notifier != nil {
			_ = bg.Notifier.Notify(ctx, msg)
		}
	}

	// 6. Wait for healthy. On failure, abort and leave the active instance
	//    untouched.
	if err := health.WaitForHealthy(ctx, tier, tierCfg, hostPort(port), proc.PID(), nil); err != nil {
		log.Errorf("new instance on slot %s failed health check: %v", inactive, err)
		// Kill the failed instance; do NOT touch the active one.
		_, _ = GracefulStop(ctx, proc, grace, bg.inFlight(bg.AppName))
		newInst.Status = "failed"
		newInst.PID = 0
		_ = Store(state)
		return bg.failf("deploy aborted: new instance unhealthy (%w); active instance untouched", err)
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
	if active == "" {
		// First deploy: enrol the app. If the proxy wasn't reachable earlier,
		// this is a hard error — we cannot offer zero-downtime without it.
		if bg.ProxyClient == nil {
			_, _ = GracefulStop(ctx, proc, grace, 0)
			newInst.Status = "failed"
			_ = Store(state)
			return errors.New("deploy aborted: no proxy client (is 'phelix proxy' running?)")
		}
		if !proxyOK {
			_, _ = GracefulStop(ctx, proc, grace, 0)
			newInst.Status = "failed"
			_ = Store(state)
			return errors.New("deploy aborted: proxy daemon unreachable (is 'phelix proxy' running?)")
		}
		if err := bg.ProxyClient.Add(ctx, bg.AppName, bg.PublicPort, primary); err != nil {
			_, _ = GracefulStop(ctx, proc, grace, 0)
			newInst.Status = "failed"
			_ = Store(state)
			return bg.failf("enrol app with proxy: %w", err)
		}
		log.Successf("enrolled %s with proxy on public port %d -> slot %s", bg.AppName, bg.PublicPort, inactive)
	} else {
		if bg.ProxyClient == nil {
			return errors.New("deploy aborted: no proxy client for switch")
		}
		if err := bg.ProxyClient.Switch(ctx, bg.AppName, primary); err != nil {
			// The new instance is healthy and running; we switch failure means
			// traffic is still on the old instance. Kill the new one and abort.
			log.Errorf("proxy switch failed: %v", err)
			_, _ = GracefulStop(ctx, proc, grace, bg.inFlight(bg.AppName))
			newInst.Status = "failed"
			_ = Store(state)
			return bg.failf("deploy aborted: proxy switch failed (%w); active instance untouched", err)
		}
		log.Successf("traffic switched to slot %s (zero downtime)", inactive)
	}

	// 8. Promote the new slot and gracefully stop the old instance.
	oldActive := active
	state.ActiveSlot = inactive
	if err := Store(state); err != nil {
		log.Warnf("failed to persist state after switch: %v (traffic already routed)", err)
	}

	if oldActive != "" {
		if old := state.Slots[oldActive]; old != nil && old.PID > 0 {
			log.Stepf("gracefully stopping old slot %s (pid %d, grace %s)", oldActive, old.PID, grace)
			// We no longer hold the *os.Process for the previous instance (it
			// was started by a prior invocation), so stop it by PID.
			report := stopByPID(ctx, old.PID, grace, bg.inFlight(bg.AppName))
			if report.ForceKilled {
				log.Warnf("old slot %s did not exit within grace; SIGKILL applied (in-flight: %d)", oldActive, report.InFlight)
			} else {
				log.Successf("old slot %s drained and exited in %s", oldActive, report.Elapsed.Round(time.Millisecond))
			}
			old.Status = "stopped"
			old.PID = 0
		}
	}

	_ = Store(state)
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
		return errors.New("no proxy client")
	}
	return bg.ProxyClient.Ping(ctx)
}

func (bg *BlueGreen) inFlight(appName string) int64 {
	if bg.InFlight == nil {
		return 0
	}
	return bg.InFlight(appName)
}

// failf wraps an error so Deploy's callers see a clear message. It also logs.
func (bg *BlueGreen) failf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	bg.logger().Errorf("%v", err)
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
func DefaultHealthProvider() HealthConfigProvider {
	return func(appID string) *health.DeployTierConfig {
		// GetConfigManager panics if not initialised; guard for the CLI path
		// where health may not have been set up yet.
		defer func() { _ = recover() }()
		cm := health.GetConfigManager()
		if cm == nil {
			return nil
		}
		cfg := cm.GetConfig(appID)
		if cfg == nil {
			return nil
		}
		return cfg.DeployTier
	}
}
