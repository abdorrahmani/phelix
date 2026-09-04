package deploy

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Rolling holds the dependencies for a rolling deploy over N replicas.
type Rolling struct {
	AppName    string
	AppID      string
	PublicPort int
	Replicas   int // desired replica count
	ExtraArgs  []string

	Builder        Builder
	Source         BuildSource
	Launcher       InstanceLauncher
	ProxyClient    ProxyClient
	HealthProvider HealthConfigProvider
	Logger         Logger
	Notifier       Notifier
	InFlight       InFlightProvider
	GracePeriod    time.Duration
	// PortHandoff, when set, is invoked right before the first proxy enrolment
	// if something still owns the public port — almost always a classic
	// instance from before the app migrated to rolling (classic → rolling
	// migration). The CLI wires this to "gracefully stop the app's own classic
	// process". It runs only AFTER the first replacement is healthy, so the
	// downtime window is the seconds between the classic process exiting and
	// the proxy binding the port.
	PortHandoff func(ctx context.Context, appName string, publicPort int) error

	// Telemetry observes the rollout lifecycle for the backend. Optional: a
	// nil Tracker is silent and changes no deployment behaviour.
	Telemetry *Tracker

	// enrolled tracks whether the app has been registered with the proxy
	// daemon during this deploy so the first healthy replica can Add instead
	// of Switch-ing against an unknown app (which silently routes nothing).
	enrolled bool
}

// Deploy restarts replicas one at a time while keeping the replica currently
// being replaced in rotation until its replacement has passed the health
// check:
//
//  1. Start the replacement for replica i on a fresh internal port ALONGSIDE
//     the current instance i (which keeps serving).
//  2. Run the tiered health check against the replacement.
//  3. Atomically update the proxy's backend set: new instance in, old one
//     out. (The first healthy replica enrolls the app instead.)
//  4. Drain and gracefully stop old instance i.
//
// This ordering makes it impossible for the proxy to route to a dead or not-
// yet-started process, which the previous stop-then-replace implementation
// did on every single replica. A health-check or proxy failure aborts the
// deploy: the failing replacement is killed, replica i keeps serving, and
// remaining replicas are untouched.
//
// Shrinking (--replicas fewer than before): surplus replicas stay serving
// until the first successful membership update excludes them, then they are
// drained and stopped at the end of the rollout.
func (r *Rolling) Deploy(ctx context.Context) (errRet error) {
	log := r.logger()
	// Rolling reports failures from many points; one deferred hook keeps the
	// telemetry's terminal event identical to what Deploy actually returned.
	defer func() { reportDeployFailure(r.Telemetry, errRet) }()
	grace := r.GracePeriod
	if grace <= 0 {
		grace = DefaultGracePeriod
	}
	if r.Replicas < 1 {
		r.Replicas = 1
	}
	// Announce before anything can fail, so every deployment the backend hears
	// about begins with a started event.
	r.Telemetry.SetReplicasDesired(r.Replicas)
	r.Telemetry.Started(activeVersionOf(nil, r.AppName), 0,
		fmt.Sprintf("rolling deploy of %s over %d replicas", r.AppName, r.Replicas))

	src := r.Source
	if src == nil && r.Builder != nil {
		src = BuilderSource(r.Builder)
	}
	if src == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: no BuildSource or Builder configured")
	}

	state, err := LoadOrInit(r.AppName, ModeRolling, r.PublicPort)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "failed to load deploy state")
	}
	state.AppID = r.AppID
	r.Telemetry.Bind(state)
	r.Telemetry.SetVersions(activeVersionOf(state, r.AppName), 0)

	// Strategy migration (e.g. blue-green → rolling): switch the recorded mode
	// up front so every subsequent persist is rolling-shaped, and keep the
	// previous strategy's instances — they keep serving until the first replica
	// is in rotation, then they are drained. If the rollout fails before it
	// replaced anything, the migration is undone below so the state again
	// describes the deployment that is actually serving.
	pre := captureMode(state)
	if pre.mode != "" && pre.mode != ModeRolling {
		log.Stepf("strategy migration: %s → rolling (retiring previous strategy's instances after first replica is healthy)", pre.mode)
	}
	legacy, err := state.MigrateTo(ModeRolling)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "persist strategy migration to rolling")
	}
	migrated := legacy != nil

	// Shrink handling: snapshot instances beyond the desired replica count so
	// they can be drained once the proxy membership no longer includes them.
	surplus := applyReplicaSet(state.Replicas, r.Replicas)
	state.Replicas = ensureReplicaMap(state.Replicas, r.Replicas)
	r.enrolled = r.checkEnrolled()

	log.Stepf("rolling deploy for %s: %d replicas", r.AppName, r.Replicas)

	// Build/prepare once — FreshBuildSource must not create a new version per replica.
	log.Stepf("%s", src.Describe())
	r.Telemetry.Building(src.Describe())
	binaryPath, envPath, err := src.Build(ctx)
	if err != nil {
		if migrated {
			r.undoMigration(state, pre, log)
		}
		return phelixerr.Wrapf(phelixerr.CodeBuildFailed, err, "prepare deploy artifact")
	}
	envOverlay, err := EnvOverlayFromSnapshot(envPath, r.AppID)
	if err != nil {
		if migrated {
			r.undoMigration(state, pre, log)
		}
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "env snapshot")
	}

	targetVer := targetVersionFromSource(src)
	r.Telemetry.SetTargetVersion(targetVer)

	tierCfg := r.healthCfg()
	// Shared tier resolution/warning with blue-green (internal/deploy/health.go).
	tier := selectDeployHealth(ctx, tierCfg, r.AppName, firstPortAddr(state), log, r.Notifier)
	r.Telemetry.SetHealthConfig(tierCfg, tier)

	// Iterate over replica indices in order.
	indices := replicaIndices(state.Replicas)
	progressed := false
	failed := false
	for _, key := range indices {
		if err := ctx.Err(); err != nil {
			failed = true
			if !progressed {
				r.undoMigration(state, pre, log)
			}
			return phelixerr.Wrapf(phelixerr.CodeDeployFailed, err, "rolling deploy cancelled before replica %s", key)
		}
		if err := r.rollOne(ctx, state, binaryPath, envOverlay, targetVer, key, tier, tierCfg, grace); err != nil {
			failed = true
			if !progressed {
				// No replacement ever reached rotation, so nothing about the
				// previous strategy's serving instance changed: restore the
				// pre-migration strategy so state and reality agree.
				r.undoMigration(state, pre, log)
			}
			return phelixerr.Wrapf(phelixerr.CodeDeployFailed, err, "rolling deploy failed at replica %s", key)
		}
		if !progressed && migrated {
			// First replacement healthy and in rotation: the previous
			// strategy's instances are out of the serving path. Drain them.
			stopRetiredInstances(ctx, legacy, grace, r.inFlight(r.AppName), log)
			legacy = nil
		}
		progressed = true
	}
	if failed {
		return phelixerr.New(phelixerr.CodeDeployFailed, "rolling deploy did not complete")
	}

	// Every replica now runs the new binary. Surplus replicas were excluded
	// from the backend set by the first membership update; retire them here so
	// a --replicas shrink never leaves orphaned old processes behind.
	r.stopSurplusReplicas(ctx, surplus, grace)

	if state.Health == nil {
		state.Health = &HealthSummary{}
	}
	state.Health.Tier = int(tier)
	state.Health.TierLabel = tier.String()
	state.Health.HealthyAt = time.Now()
	if ver := targetVersionFromSource(src); ver > 0 {
		state.ActiveVersion = ver
		if err := PromoteVersion(r.AppName, ver, string(ModeRolling)); err != nil {
			log.Warnf("failed to promote version v%d: %v", ver, err)
		}
	}
	// Every replica is in rotation on the new binary: the target version is now
	// the one serving traffic.
	r.Telemetry.PromoteCurrentVersion()
	if err := Store(state); err != nil {
		log.Warnf("failed to persist rolling deploy state: %v (traffic already routed)", err)
	}

	log.Successf("rolling deploy of %s complete: %d replicas updated", r.AppName, len(indices))
	r.Telemetry.Completed(fmt.Sprintf("%d replicas updated", len(indices)))
	return nil
}

// handOffPublicPort frees the public port before the first proxy enrolment
// during a classic → rolling migration: the CLI-wired PortHandoff stops the
// classic instance that still binds the port. The replacement is already
// healthy at this point, so the window is only the handoff itself.
func (r *Rolling) handOffPublicPort(ctx context.Context, log Logger) error {
	if r.PortHandoff == nil {
		return phelixerr.Newf(phelixerr.CodePortUnavailable,
			"public port %d is owned by another process (classic instance?); stop it once, then deploy",
			r.PublicPort)
	}
	log.Stepf("public port %d is owned by a classic instance; handing it to the proxy", r.PublicPort)
	if err := r.PortHandoff(ctx, r.AppName, r.PublicPort); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeProxy, err, "classic-to-rolling handoff failed")
	}
	if !waitForPortRelease(r.PublicPort, 5*time.Second) {
		return phelixerr.Newf(phelixerr.CodePortUnavailable,
			"public port %d still bound after stopping the classic instance", r.PublicPort)
	}
	return nil
}

// undoMigration reverts a strategy migration whose rollout failed before it
// replaced anything, so deploy.json again describes the deployment that is
// still actually serving (e.g. the blue-green slot that owns the proxy).
// For a first-ever deploy attempt there was no previous strategy to restore:
// the half-created deploy.json is removed so status/list/reconciliation fall
// back to the classic app record that still describes reality.
func (r *Rolling) undoMigration(state *DeployState, pre modeSnapshot, log Logger) {
	if pre.mode == "" || pre.mode == ModeRolling {
		if pre.mode == "" {
			if err := RemoveState(r.AppName); err != nil {
				log.Warnf("failed to remove half-created deploy state: %v", err)
			}
		}
		return
	}
	state.restoreMode(pre)
	if err := Store(state); err != nil {
		log.Warnf("failed to persist strategy migration rollback: %v", err)
	}
}

// rollOne replaces exactly one replica while it keeps serving. Ordering rules
// (see Rolling.Deploy): start alongside → health-check replacement → swap
// membership → drain old instance.
func (r *Rolling) rollOne(ctx context.Context, state *DeployState, binaryPath string, envOverlay []string, targetVer int, key string, tier health.Tier, tierCfg *health.DeployTierConfig, grace time.Duration) error {
	log := r.logger()
	// Replica identity is the index deploy.json already keys instances by, so
	// telemetry, the proxy label ("replica-N") and the state file all agree.
	index, _ := strconv.Atoi(key)

	// Snapshot whatever serves this replica today.
	oldPID, oldBinary := 0, ""
	if cur := state.Replicas[key]; cur != nil {
		oldPID, oldBinary = cur.PID, cur.BinaryPath
	} else {
		state.Replicas[key] = &Instance{Slot: key, Status: "stopped"}
	}

	// 1. Start the replacement ALONGSIDE the current instance.
	if oldPID > 0 {
		log.Stepf("replica %s: starting replacement alongside pid %d", key, oldPID)
	} else {
		log.Stepf("replica %s: starting first instance from %s", key, binaryPath)
	}
	proc, port, err := r.Launcher(ctx, binaryPath, envOverlay)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "start replica %s replacement", key)
	}
	newPID := proc.PID()
	inst := state.Replicas[key]
	if oldPID > 0 {
		// The old instance is STILL serving; mark only that a replacement is
		// in flight without pretending the replica died.
		inst.Status = "replacing"
		_ = Store(state)
	}
	r.Telemetry.ReplicaStarted(index, newPID, port)

	// 2. Health-check ONLY the replacement's port.
	r.Telemetry.ReplicaHealthCheckStarted(index, port)
	if err := health.WaitForHealthy(ctx, tier, tierCfg, hostPort(port), newPID, nil); err != nil {
		stopHeldProcess(ctx, proc, grace)
		inst.Status = "failed"
		inst.PID = oldPID // revert to what actually still serves this replica
		if err := Store(state); err != nil {
			log.Warnf("replica %s: persist failure after unhealthy replacement: %v", key, err)
		}
		return phelixerr.Wrapf(phelixerr.CodeHealthCheckFailed,
			candidateHealthFailure(binaryPath, port, err),
			"replica %s replacement failed health check; previous instance still serving", key)
	}

	// 3. Membership swap FIRST: new instance in, old instance out.
	r.Telemetry.ReplicaHealthy(index, newPID, port)
	primary, backends := membershipAfterReplace(state.Replicas, key, port)
	if r.ProxyClient == nil {
		stopHeldProcess(ctx, proc, grace)
		inst.Status = "failed"
		inst.PID = oldPID
		_ = Store(state)
		return phelixerr.Newf(phelixerr.CodeProxy, "replica %s: no proxy client configured", key)
	}
	var proxyErr error
	r.Telemetry.ReplicaProxySwitching(index, port,
		fmt.Sprintf("replica %s entering rotation on public port %d", key, r.PublicPort))
	if !r.enrolled {
		// Classic → rolling migration: the public port may still be owned by
		// the app's classic instance. Free it before the proxy can bind — but
		// only when a handoff callback is wired; without one the subsequent
		// Add fails with its own (clear) bind error.
		if r.PortHandoff != nil && r.PublicPort > 0 && !phelixport.IsAvailable(r.PublicPort) {
			if err := r.handOffPublicPort(ctx, log); err != nil {
				stopHeldProcess(ctx, proc, grace)
				inst.Status = "failed"
				inst.PID = oldPID
				_ = Store(state)
				return err
			}
		}
		proxyErr = r.ProxyClient.Add(ctx, r.AppName, r.PublicPort, primary, backends...)
		if proxyErr == nil {
			r.enrolled = true
			log.Successf("enrolled %s with proxy on public port %d", r.AppName, r.PublicPort)
		}
	} else {
		proxyErr = r.ProxyClient.Switch(ctx, r.AppName, primary, backends...)
	}
	if proxyErr != nil {
		// Replica i never left rotation — no customer impact. Kill just the
		// replacement we started.
		stopHeldProcess(ctx, proc, grace)
		inst.Status = "failed"
		inst.PID = oldPID
		if err := Store(state); err != nil {
			log.Warnf("replica %s: persist failure after proxy error: %v", key, err)
		}
		return phelixerr.Wrapf(phelixerr.CodeProxy, proxyErr,
			"replica %s: proxy membership update failed; previous instance still serving", key)
	}

	// 4. Old instance is out of rotation: drain and stop it. Identity of the
	// PID is verified against its recorded executable path first.
	r.Telemetry.SetProxy(r.PublicPort, primary.Label, port, upstreamHosts(primary, backends))
	r.Telemetry.ReplicaReplaced(index, newPID, port,
		fmt.Sprintf("proxy primary %s, %d upstream(s)", primary.Label, len(backends)+1))
	if oldPID > 0 {
		r.Telemetry.ReplicaDraining(index, oldPID)
		report := stopByPID(ctx, oldPID, grace, r.inFlight(r.AppName), oldBinary)
		if report.ForceKilled {
			log.Warnf("replica %s: old instance (pid %d) ignored SIGTERM; SIGKILL applied", key, oldPID)
		} else if report.Exited {
			log.Stepf("replica %s: old instance drained and stopped", key)
		}
		r.Telemetry.ReplicaStopped(index, oldPID, drainOutcome(report))
	}

	inst.PID = newPID
	inst.Port = port
	inst.BinaryPath = binaryPath
	inst.StartedAt = time.Now()
	inst.Version = targetVer
	inst.Status = "running"
	if err := Store(state); err != nil {
		log.Warnf("replica %s: failed to persist deploy state: %v", key, err)
	}
	log.Successf("replica %s: healthy and in rotation (pid %d, port %d)", key, inst.PID, inst.Port)
	return nil
}

func (r *Rolling) logger() Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return &nopLogger{}
}

func (r *Rolling) healthCfg() *health.DeployTierConfig {
	if r.HealthProvider != nil {
		return r.HealthProvider(r.AppID)
	}
	return nil
}

func (r *Rolling) inFlight(appName string) int64 {
	if r.InFlight == nil {
		return 0
	}
	return r.InFlight(appName)
}

// --- replica helpers -------------------------------------------------------

// ensureReplicaMap returns a replica map sized to n, preserving any existing
// instances for indices < n and dropping indices >= n.
func ensureReplicaMap(existing map[string]*Instance, n int) map[string]*Instance {
	out := make(map[string]*Instance, n)
	for i := 0; i < n; i++ {
		key := strconv.Itoa(i)
		if inst, ok := existing[key]; ok {
			out[key] = inst
		} else {
			out[key] = &Instance{Slot: key, Status: "stopped"}
		}
	}
	return out
}

func replicaIndices(m map[string]*Instance) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, _ := strconv.Atoi(out[i])
		aj, _ := strconv.Atoi(out[j])
		return ai < aj
	})
	return out
}

// applyReplicaSet removes replica entries beyond the desired count and
// returns their Instance records so they can be drained after leaving the
// proxy's rotation. Existing instances for indices < n are preserved.
func applyReplicaSet(existing map[string]*Instance, n int) []*Instance {
	var surplus []*Instance
	for k, inst := range existing {
		idx, err := strconv.Atoi(k)
		if err != nil || idx >= n {
			if inst != nil && inst.PID > 0 {
				surplus = append(surplus, inst)
			}
			delete(existing, k)
		}
	}
	sort.Slice(surplus, func(i, j int) bool { return surplus[i].Slot < surplus[j].Slot })
	return surplus
}

// stopSurplusReplicas drains instances removed by a --replicas shrink. By the
// time this runs, a successful membership update has already excluded them.
func (r *Rolling) stopSurplusReplicas(ctx context.Context, surplus []*Instance, grace time.Duration) {
	if len(surplus) == 0 {
		return
	}
	log := r.logger()
	for _, inst := range surplus {
		if inst == nil || inst.PID <= 0 {
			continue
		}
		log.Stepf("stopping surplus replica %s (pid %d) removed by shrink", inst.Slot, inst.PID)
		report := stopByPID(ctx, inst.PID, grace, 0, inst.BinaryPath)
		if report.ForceKilled {
			log.Warnf("surplus replica %s ignored SIGTERM; SIGKILL applied", inst.Slot)
		}
		inst.PID = 0
		inst.Status = "stopped"
	}
}

// checkEnrolled asks the daemon whether this app is already registered so the
// rollout knows to Add before its first Switch. Unknown/unreachable daemon
// states conservatively report "not enrolled" — the first Add will then fail
// with a clear error instead of switches silently routing nothing.
func (r *Rolling) checkEnrolled() bool {
	if r.ProxyClient == nil {
		return false
	}
	st, err := r.ProxyClient.Status(context.Background(), r.AppName)
	return err == nil && len(st) > 0
}

// membershipAfterReplace computes the proxy primary+backends given that
// replica key now listens on newPort: every other healthy replica stays in
// the set, nothing points at the replaced port any more.
func membershipAfterReplace(replicas map[string]*Instance, key string, newPort int) (proxy.Target, []proxy.Target) {
	virtual := make(map[string]*Instance, len(replicas))
	for k, v := range replicas {
		if k == key {
			virtual[k] = &Instance{Slot: key, Status: "running", Port: newPort}
			continue
		}
		virtual[k] = v
	}
	return healthyTargets(virtual)
}

// stopHeldProcess gracefully stops an instance whose Process handle we still
// hold (used to kill unproven replacements without touching the serving one).
func stopHeldProcess(ctx context.Context, proc Process, grace time.Duration) {
	if proc == nil {
		return
	}
	_, _ = GracefulStop(ctx, proc, grace, 0)
}

// healthyTargets returns (primary, backends) over all healthy replicas. The
// primary is the lowest-index healthy replica. Backends exclude the primary —
// the wire contract (EnrollApp/SwitchApp) treats primary and backends as
// disjoint sets; the daemon unions them. Sending the primary inside backends
// too made old daemons (pre-dedupe SetTarget) persist it twice.
func healthyTargets(replicas map[string]*Instance) (proxy.Target, []proxy.Target) {
	indices := replicaIndices(replicas)
	var backends []proxy.Target
	var primary proxy.Target
	for _, k := range indices {
		inst := replicas[k]
		if inst == nil || inst.Status != "running" || inst.Port == 0 {
			continue
		}
		t := proxy.Target{Host: hostPort(inst.Port), Label: "replica-" + k}
		if primary.Host == "" {
			primary = t
			continue
		}
		backends = append(backends, t)
	}
	if primary.Host == "" {
		primary = proxy.Target{Host: "127.0.0.1:0", Label: "none"}
	}
	return primary, backends
}

// firstPortAddr returns the host:port of the first replica with a port, for
// tier auto-selection. Returns a non-routable address when none is running.
func firstPortAddr(state *DeployState) string {
	if state == nil || state.Replicas == nil {
		return "127.0.0.1:0"
	}
	for _, k := range replicaIndices(state.Replicas) {
		if inst := state.Replicas[k]; inst != nil && inst.Port != 0 {
			return hostPort(inst.Port)
		}
	}
	return "127.0.0.1:0"
}
