package deploy

import (
	"context"
	"os"
	"sort"
	"strconv"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// This file owns the lifecycle operations on an existing DeployState —
// verifying reality (InstanceAlive/ServingInstance), tearing a deployment
// down (stop) and restoring it (start). It exists so `phelix stop/start/
// restart` can operate on proxy-managed apps without falling back to the
// classic single-PID flow, which would bind the public port directly and
// fight the proxy for ownership.

// InstanceAlive reports whether inst's recorded process is alive AND still
// resolves to the binary recorded for it. The executable check is what makes
// a recycled PID unusable as "the instance is running" evidence.
func InstanceAlive(inst *Instance) bool {
	if inst == nil || inst.PID <= 0 {
		return false
	}
	return findVerifiedProcess(inst.PID, inst.BinaryPath) != nil
}

// ServingInstance returns the instance that should be serving traffic right
// now: the active slot for blue-green, or the first live replica for rolling.
// It returns nil when nothing is (or should be) serving.
func (s *DeployState) ServingInstance() *Instance {
	if s == nil {
		return nil
	}
	switch s.Mode {
	case ModeBlueGreen:
		return s.ActiveInstance()
	case ModeRolling:
		keys := sortedReplicaKeys(s.Replicas)
		for _, k := range keys {
			if inst := s.Replicas[k]; inst != nil && inst.PID > 0 {
				return inst
			}
		}
	}
	return nil
}

func sortedReplicaKeys(m map[string]*Instance) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, aerr := strconv.Atoi(keys[i])
		b, berr := strconv.Atoi(keys[j])
		if aerr == nil && berr == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}

// LifecycleDecision is the application lifecycle status derived from
// deployment reality rather than from what deploy.json claims.
type LifecycleDecision struct {
	// Running is true only when the instance that should be serving traffic
	// is alive and identity-verified. A state file naming an active slot is
	// never sufficient on its own.
	Running bool
	// PID is the serving process id (0 when not running).
	PID int
	// StartedAt is when the serving instance was launched (zero when unknown).
	StartedAt time.Time
	// PublicPort is the proxy-owned public port (0 when unknown).
	PublicPort int
}

// DecideLifecycle derives LifecycleDecision from a DeployState. Callers use
// it to keep the application lifecycle record consistent with the deployment
// instead of maintaining two independent, drifting notions of "running".
func DecideLifecycle(state *DeployState) LifecycleDecision {
	if state == nil {
		return LifecycleDecision{}
	}
	inst := state.ServingInstance()
	if !InstanceAlive(inst) {
		return LifecycleDecision{PublicPort: state.PublicPort}
	}
	d := LifecycleDecision{
		Running:    true,
		PID:        inst.PID,
		PublicPort: state.PublicPort,
	}
	if !inst.StartedAt.IsZero() {
		d.StartedAt = inst.StartedAt
	}
	return d
}

// TeardownDeployment stops a proxy-managed deployment: every recorded
// instance (verified by executable path, never by bare PID) is gracefully
// stopped and the persisted records are marked stopped. Historical metadata
// (active slot, versions, health tier) is preserved so a later
// StartDeployment can restore the deployment as it was.
//
// Removing the proxy route is the caller's job (it needs the CLI's proxy
// client); TeardownDeployment only owns processes and deploy.json.
func TeardownDeployment(ctx context.Context, state *DeployState, grace time.Duration, log Logger) error {
	if state == nil || state.AppName == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: teardown requires app state")
	}
	if log == nil {
		log = &nopLogger{}
	}
	if grace <= 0 {
		grace = DefaultGracePeriod
	}

	stopAll := func(instances map[string]*Instance) {
		for _, inst := range instances {
			if inst == nil || inst.PID <= 0 {
				continue
			}
			if !InstanceAlive(inst) {
				// Stale record (dead PID or recycled): just clear it.
				inst.Status = "stopped"
				inst.PID = 0
				continue
			}
			log.Stepf("stopping %s instance pid %d (grace %s)", state.AppName, inst.PID, grace)
			report := stopByPID(ctx, inst.PID, grace, 0, inst.BinaryPath)
			if report.ForceKilled {
				log.Warnf("instance pid %d ignored SIGTERM; SIGKILL applied", inst.PID)
			}
			inst.Status = "stopped"
			inst.PID = 0
		}
	}
	stopAll(state.Slots)
	stopAll(state.Replicas)

	if err := Store(state); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: persist teardown state for %s", state.AppName)
	}
	return nil
}

// StartOptions configures StartDeployment.
type StartOptions struct {
	// Launcher starts an instance; DefaultLauncher is used when nil.
	Launcher InstanceLauncher
	// ListenTimeout bounds the wait for a restored instance to bind its
	// internal port. Defaults to 10s.
	ListenTimeout time.Duration
	// Logger receives progress lines; nil discards them.
	Logger Logger
}

// StartDeployment restores a stopped blue-green/rolling deployment from its
// persisted state: each instance that should be serving (the active slot, or
// every recorded replica) is relaunched from its recorded binary on a fresh
// internal port. The caller is responsible for re-establishing the proxy
// route (it needs the proxy client) and for reconciling the application
// lifecycle record.
func StartDeployment(ctx context.Context, state *DeployState, opts StartOptions) error {
	if state == nil || state.AppName == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: start requires app state")
	}
	if opts.Launcher == nil {
		opts.Launcher = DefaultLauncher
	}
	if opts.ListenTimeout <= 0 {
		opts.ListenTimeout = 10 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = &nopLogger{}
	}

	var toStart []*Instance
	switch state.Mode {
	case ModeBlueGreen:
		inst := state.ActiveInstance()
		if inst == nil || inst.BinaryPath == "" {
			return phelixerr.Newf(phelixerr.CodeNotFound,
				"no deployable instance recorded for %s (active slot %q); run 'phelix rebuild %s --blue-green' first",
				state.AppName, state.ActiveSlot, state.AppName)
		}
		toStart = []*Instance{inst}
	case ModeRolling:
		if len(state.Replicas) == 0 {
			return phelixerr.Newf(phelixerr.CodeNotFound,
				"no replicas recorded for %s; run 'phelix rebuild %s --replicas N' first",
				state.AppName, state.AppName)
		}
		for _, k := range sortedReplicaKeys(state.Replicas) {
			if inst := state.Replicas[k]; inst != nil && inst.BinaryPath != "" {
				toStart = append(toStart, inst)
			}
		}
		if len(toStart) == 0 {
			return phelixerr.Newf(phelixerr.CodeNotFound, "no deployable replicas recorded for %s", state.AppName)
		}
	default:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unsupported deploy mode %q", state.Mode)
	}

	if _, err := os.Stat(toStart[0].BinaryPath); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeNotFound, err, "deploy: recorded binary for %s is gone", state.AppName)
	}

	for _, inst := range toStart {
		envOverlay, err := EnvOverlayFromSnapshot(inst.EnvPath, state.AppID)
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "deploy: load env snapshot for %s", state.AppName)
		}
		log.Stepf("starting %s instance on slot %s", state.AppName, inst.Slot)
		proc, newPort, err := opts.Launcher(ctx, inst.BinaryPath, envOverlay)
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "deploy: start %s instance", state.AppName)
		}
		addr := hostPort(newPort)
		if !port.WaitForListener(addr, opts.ListenTimeout) {
			_, _ = GracefulStop(ctx, proc, 3*time.Second, 0)
			return phelixerr.Newf(phelixerr.CodePortUnavailable,
				"restored %s instance did not listen on %s within %s; see ~/.phelix/logs",
				state.AppName, addr, opts.ListenTimeout)
		}
		inst.PID = proc.PID()
		inst.Port = newPort
		inst.Status = "running"
		inst.StartedAt = time.Now()
		log.Successf("instance pid %d listening on %s", inst.PID, addr)
	}

	if err := Store(state); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: persist start state for %s", state.AppName)
	}
	return nil
}

// RestoreProxyRoute re-establishes the proxy route for an app whose instances
// were just restored by StartDeployment. Apps already enrolled get a Switch;
// unenrolled apps get an Add (first restore after a proxy wipe).
func RestoreProxyRoute(ctx context.Context, state *DeployState, pc ProxyClient, log Logger) error {
	if pc == nil {
		return phelixerr.New(phelixerr.CodeProxy, "no proxy client")
	}
	if log == nil {
		log = &nopLogger{}
	}
	var primary proxy.Target
	var backends []proxy.Target
	switch state.Mode {
	case ModeBlueGreen:
		inst := state.ActiveInstance()
		if inst == nil || inst.PID <= 0 {
			return phelixerr.Newf(phelixerr.CodeNotFound, "no running instance for %s", state.AppName)
		}
		primary = proxy.Target{Host: hostPort(inst.Port), Label: inst.Slot}
	case ModeRolling:
		for _, k := range sortedReplicaKeys(state.Replicas) {
			inst := state.Replicas[k]
			if inst == nil || inst.PID <= 0 {
				continue
			}
			t := proxy.Target{Host: hostPort(inst.Port), Label: inst.Slot}
			if primary.Host == "" {
				primary = t
			}
			backends = append(backends, t)
		}
		if primary.Host == "" {
			return phelixerr.Newf(phelixerr.CodeNotFound, "no running replicas for %s", state.AppName)
		}
	default:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument, "unsupported deploy mode %q", state.Mode)
	}

	enrolled := false
	if st, err := pc.Status(ctx, state.AppName); err == nil && len(st) > 0 {
		enrolled = true
	}
	if enrolled {
		if err := pc.Switch(ctx, state.AppName, primary, backends...); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeProxy, err, "proxy switch failed for %s", state.AppName)
		}
		log.Successf("proxy route restored: :%d -> %s", state.PublicPort, primary.Label)
		return nil
	}
	if err := pc.Add(ctx, state.AppName, state.PublicPort, primary, backends...); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProxy, err, "proxy enrol failed for %s", state.AppName)
	}
	log.Successf("proxy route restored: :%d -> %s", state.PublicPort, primary.Label)
	return nil
}

// waitForPortRelease polls until the port is bindable again (the previous
// owner exited and the kernel released the socket), or the timeout elapses.
func waitForPortRelease(publicPort int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if port.IsAvailable(publicPort) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// handOffPublicPort frees the public port before the first proxy enrolment.
// A classic instance (started before the app migrated to blue-green) binds
// the public port directly; the proxy cannot take ownership while that
// process lives. The handoff callback (wired by the CLI layer) stops the
// classic instance — after the replacement is already healthy, so the
// downtime window is only the seconds between the old process exiting and
// the proxy binding the port.
func (bg *BlueGreen) handOffPublicPort(ctx context.Context, proc Process, log Logger) error {
	if bg.PublicPort <= 0 || bg.PortHandoff == nil {
		return nil
	}
	if port.IsAvailable(bg.PublicPort) {
		return nil // nobody owns it — nothing to hand off
	}
	log.Stepf("public port %d is owned by a classic instance; handing it to the proxy", bg.PublicPort)
	if err := bg.PortHandoff(ctx, bg.AppName, bg.PublicPort); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProxy, err, "classic-to-blue-green handoff failed")
	}
	if !waitForPortRelease(bg.PublicPort, 5*time.Second) {
		return phelixerr.Newf(phelixerr.CodePortUnavailable,
			"public port %d still bound after stopping the classic instance", bg.PublicPort)
	}
	return nil
}
