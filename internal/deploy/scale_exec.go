package deploy

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// ScaleProgress is the deploy.json marker of an automatic scaling operation
// in flight, mirroring the WorkerRollout/Canary crash-marker convention. It
// carries everything RecoverInterruptedScale needs to converge state, proxy
// membership and processes after a monitor crash mid-scale: for a scale-up
// the not-yet-recorded replica's PID, for a scale-down the victim's identity
// so it can still be drained after it left the replica map.
type ScaleProgress struct {
	Direction string    `json:"direction"`
	From      int       `json:"from"`
	To        int       `json:"to"`
	StartedAt time.Time `json:"started_at"`

	NewPID int `json:"new_pid,omitempty"`

	VictimKey    string `json:"victim_key,omitempty"`
	VictimPID    int    `json:"victim_pid,omitempty"`
	VictimPort   int    `json:"victim_port,omitempty"`
	VictimBinary string `json:"victim_binary,omitempty"`
}

// ScaleExecutor applies one automatic scaling decision to a rolling
// deployment without rebuilding: scale-ups launch the active version's
// already-built artifact, scale-downs exclude the victim from the proxy
// first and drain it after the state commit. Every mutation happens under
// the app's deploy lock, shared with deploys, rollbacks, manual scaling and
// the supervisor. It reuses the rolling engine's launcher, health gate,
// membership helpers, drain and persistence discipline; it introduces no new
// launcher, proxy protocol or state store.
type ScaleExecutor struct {
	AppName    string
	AppID      string
	PublicPort int

	Launcher       InstanceLauncher
	ProxyClient    ProxyClient
	HealthProvider HealthConfigProvider
	Logger         Logger
	InFlight       InFlightProvider
	GracePeriod    time.Duration

	// stateStore is a test seam; production leaves it nil and uses Store.
	stateStore func(*DeployState) error
}

// Scale moves the rolling replica fleet from current to desired under the
// app lock. A stale decision (the live count no longer matches the count the
// decision was made on) is rejected without side effects so the next
// evaluation re-decides against reality. Scaling is refused while a canary
// rollout, a worker rollout or a secret transaction is in flight: the autoscaler
// never interferes with a version change.
func (e *ScaleExecutor) Scale(ctx context.Context, current, desired int) error {
	if e.ProxyClient == nil {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "scale executor: no proxy client configured")
	}
	release, err := (ReplicaCoordinator{}).LockApp(ctx, e.AppName)
	if err != nil {
		return err
	}
	defer release()

	state, err := loadStateForScale(e.AppName)
	if err != nil {
		return err
	}
	if state.Mode != ModeRolling {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"autoscale: %s is not a rolling deployment (mode %s)", e.AppName, state.Mode)
	}
	if state.Canary != nil && state.Canary.Status == CanaryRunning {
		return phelixerr.Newf(phelixerr.CodeDeployFailed,
			"autoscale: %s has a canary rollout in flight; scaling is deferred", e.AppName)
	}
	if state.WorkerRollout != nil && state.WorkerRollout.Status == WorkerRolloutRunning {
		return phelixerr.Newf(phelixerr.CodeDeployFailed,
			"autoscale: %s has a worker rollout in flight; scaling is deferred", e.AppName)
	}
	from := len(state.Replicas)
	if from != current {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"autoscale: stale decision for %s: decided on %d replicas, %d recorded", e.AppName, current, from)
	}
	if desired == from {
		return nil
	}
	if desired > from {
		return e.scaleUp(ctx, state, from, desired)
	}
	return e.scaleDown(ctx, state, from, desired)
}

// scaleUp launches desired-from replicas of the active version, health-gates
// each, adds it to the proxy membership and commits state per replica. The
// marker is written before the first launch and cleared after the last
// durable write so any crash converges on recovery.
func (e *ScaleExecutor) scaleUp(ctx context.Context, state *DeployState, from, desired int) error {
	log := e.logger()
	grace := e.grace()

	ver := state.ActiveVersion
	if ver <= 0 {
		return phelixerr.Newf(phelixerr.CodeConfiguration,
			"autoscale: %s has no active version to scale; run a build or rebuild first", e.AppName)
	}
	binaryPath, envPath, err := VersionPaths(e.AppName, ver)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
			"autoscale: resolve artifact for %s v%d", e.AppName, ver)
	}
	envOverlay, releaseOverlay, err := launchOverlay(envPath, e.AppID, nil)
	defer releaseOverlay()
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "autoscale: env snapshot for %s", e.AppName)
	}

	tier, tierCfg := e.resolveHealth(ctx, state, log)
	enrolled := e.checkEnrolled()

	state.Replicas = ensureReplicaMap(state.Replicas, desired)
	state.ScaleProgress = &ScaleProgress{Direction: "up", From: from, To: desired, StartedAt: time.Now()}
	if err := e.persist(state); err != nil {
		state.Replicas = ensureReplicaMap(state.Replicas, from)
		return err
	}

	for i := from; i < desired; i++ {
		key := strconv.Itoa(i)
		if err := e.scaleUpOne(ctx, state, key, binaryPath, envPath, envOverlay, ver, tier, tierCfg, grace, enrolled); err != nil {
			return err
		}
	}

	state.ScaleProgress = nil
	return e.persist(state)
}

// scaleUpOne launches, health-checks and enrolls exactly one new replica,
// with the same compensation discipline as a rolling replace: a failure at
// any point stops the candidate, restores the replica map and the proxy, and
// persists the restored state.
func (e *ScaleExecutor) scaleUpOne(ctx context.Context, state *DeployState, key, binaryPath, envPath string, envOverlay []string, ver int, tier health.Tier, tierCfg *health.DeployTierConfig, grace time.Duration, enrolled bool) error {
	log := e.logger()

	proc, port, err := e.Launcher(ctx, binaryPath, envOverlay)
	if err != nil {
		e.clearProgress(state)
		if persistErr := e.persist(state); persistErr != nil {
			log.Warnf("autoscale: persist after launch failure: %v", persistErr)
		}
		return phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "autoscale: launch replica %s", key)
	}
	state.ScaleProgress.NewPID = proc.PID()
	if err := e.persist(state); err != nil {
		stopHeldProcess(ctx, proc, grace)
		e.clearProgress(state)
		_ = e.persist(state)
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "autoscale: persist scale marker for replica %s", key)
	}

	inst := state.Replicas[key]
	inst.BinaryPath = binaryPath
	inst.EnvPath = envPath
	inst.Version = ver
	inst.StartTicks = startTicksOf(proc)
	inst.ContainerID = containerIDOf(proc)
	inst.StartedAt = time.Now()
	inst.PID = proc.PID()
	inst.Port = port
	inst.Status = "starting"

	if err := health.WaitForHealthy(ctx, tier, tierCfg, hostPort(port), proc.PID(), deployResolver(proc)); err != nil {
		stopHeldProcess(ctx, proc, grace)
		delete(state.Replicas, key)
		e.clearProgress(state)
		if persistErr := e.persist(state); persistErr != nil {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, persistErr,
				"autoscale: replica %s failed health check and the state restore also failed", key)
		}
		return oomFailure(proc,
			phelixerr.Wrapf(phelixerr.CodeHealthCheckFailed, err,
				"autoscale: replica %s failed health check", key),
			fmt.Sprintf("autoscale: replica %s exceeded its configured memory limit and was killed by the kernel OOM killer", key))
	}
	inst.Status = "running"
	inst.ReadyAt = time.Now()

	primary, backends := healthyTargets(state.Replicas)
	if !enrolled {
		err = e.ProxyClient.Add(ctx, e.AppName, e.PublicPort, primary, backends...)
		enrolled = err == nil
	} else {
		err = e.ProxyClient.Switch(ctx, e.AppName, primary, backends...)
	}
	if err != nil {
		stopHeldProcess(ctx, proc, grace)
		delete(state.Replicas, key)
		e.clearProgress(state)
		if persistErr := e.persist(state); persistErr != nil {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, persistErr,
				"autoscale: replica %s proxy update failed and the state restore also failed", key)
		}
		return phelixerr.Wrapf(phelixerr.CodeProxy, err, "autoscale: replica %s proxy membership update failed", key)
	}

	// Commit the new fleet. If the commit fails, revert the proxy to the
	// pre-scale membership and stop the candidate: the proxy must never route
	// to a replica deploy.json does not record.
	if err := e.persist(state); err != nil {
		state.Replicas = ensureReplicaMap(state.Replicas, len(state.Replicas)-1)
		oldPrimary, oldBackends := healthyTargets(state.Replicas)
		var compErr error
		if enrolled {
			compErr = e.ProxyClient.Switch(ctx, e.AppName, oldPrimary, oldBackends...)
		} else {
			compErr = e.ProxyClient.Remove(ctx, e.AppName)
		}
		stopHeldProcess(ctx, proc, grace)
		e.clearProgress(state)
		if persistErr := e.persist(state); persistErr != nil {
			log.Warnf("autoscale: persist restored fleet: %v", persistErr)
		}
		if compErr != nil {
			return phelixerr.Wrapf(phelixerr.CodeProxy, compErr,
				"autoscale: replica %s state commit failed and proxy compensation failed", key)
		}
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "autoscale: replica %s state commit failed; proxy and fleet restored", key)
	}
	log.Successf("autoscale: replica %s healthy and in rotation (pid %d, port %d)", key, proc.PID(), port)
	return nil
}

// scaleDown removes desired-from replicas. Ordering is the rolling shrink
// invariant: victims keep serving until a successful membership update
// excludes them, the fleet commit is durable before anything is stopped, and
// only then are victims drained.
func (e *ScaleExecutor) scaleDown(ctx context.Context, state *DeployState, from, desired int) error {
	log := e.logger()
	grace := e.grace()

	surplus := applyReplicaSet(state.Replicas, desired)
	state.Replicas = ensureReplicaMap(state.Replicas, desired)

	enrolled := e.checkEnrolled()
	if enrolled {
		primary, backends := healthyTargets(state.Replicas)
		if err := e.ProxyClient.Switch(ctx, e.AppName, primary, backends...); err != nil {
			// Restore the removed records: they are still serving.
			for _, inst := range surplus {
				if inst != nil {
					state.Replicas[inst.Slot] = inst
				}
			}
			return phelixerr.Wrapf(phelixerr.CodeProxy, err, "autoscale: exclude surplus replicas from proxy membership")
		}
	}

	state.ScaleProgress = &ScaleProgress{Direction: "down", From: from, To: desired, StartedAt: time.Now()}
	for _, inst := range surplus {
		if inst == nil {
			continue
		}
		if state.ScaleProgress.VictimKey == "" {
			state.ScaleProgress.VictimKey = inst.Slot
			state.ScaleProgress.VictimPID = inst.PID
			state.ScaleProgress.VictimPort = inst.Port
			state.ScaleProgress.VictimBinary = inst.BinaryPath
		}
	}
	if err := e.persist(state); err != nil {
		// Membership may already exclude the victims; re-include them so the
		// proxy and deploy.json agree while the fleet is unchanged.
		if enrolled {
			oldPrimary, oldBackends := healthyTargets(state.Replicas)
			_ = e.ProxyClient.Switch(ctx, e.AppName, oldPrimary, oldBackends...)
		}
		for _, inst := range surplus {
			if inst != nil {
				state.Replicas[inst.Slot] = inst
			}
		}
		e.clearProgress(state)
		return err
	}

	for _, inst := range surplus {
		if inst == nil || (inst.PID <= 0 && inst.ContainerID == "") {
			continue
		}
		report := stopInstance(ctx, inst, grace, e.inFlight())
		if report.ForceKilled {
			log.Warnf("autoscale: surplus replica %s ignored SIGTERM; SIGKILL applied", inst.Slot)
		} else if report.Exited {
			log.Stepf("autoscale: surplus replica %s drained and stopped", inst.Slot)
		}
	}

	state.ScaleProgress = nil
	return e.persist(state)
}

// RecoverInterruptedScale converges a scaling operation that was interrupted
// by a monitor crash. It runs under the app lock (the caller acquires it)
// and follows the webhook recovery principle: corroborate with external
// evidence, never invent success.
func RecoverInterruptedScale(ctx context.Context, appName string, proxyClient ProxyClient) error {
	release, err := (ReplicaCoordinator{}).LockApp(ctx, appName)
	if err != nil {
		return err
	}
	defer release()

	state, err := Load(appName)
	if err != nil {
		return err
	}
	marker := state.ScaleProgress
	if marker == nil {
		return nil
	}

	switch marker.Direction {
	case "up":
		if len(state.Replicas) >= marker.To {
			// The final commit landed; only the marker clear was lost.
			state.ScaleProgress = nil
			return Store(state)
		}
		// Replicas the fleet never recorded are orphans: stop them and
		// re-sync the proxy with the recorded fleet.
		if err := stopUnrecordedReplicas(ctx, state, marker); err != nil {
			return err
		}
		if proxyClient != nil {
			if primary, backends := healthyTargets(state.Replicas); len(state.Replicas) > 0 {
				_ = proxyClient.Switch(ctx, appName, primary, backends...)
			}
		}
		state.ScaleProgress = nil
		return Store(state)

	case "down":
		if marker.VictimPID > 0 || marker.VictimPort > 0 {
			victim := &Instance{
				Slot:       marker.VictimKey,
				PID:        marker.VictimPID,
				Port:       marker.VictimPort,
				BinaryPath: marker.VictimBinary,
			}
			stopInstance(ctx, victim, DefaultGracePeriod, 0)
		}
		if proxyClient != nil && len(state.Replicas) > 0 {
			if primary, backends := healthyTargets(state.Replicas); len(state.Replicas) > 0 {
				_ = proxyClient.Switch(ctx, appName, primary, backends...)
			}
		}
		state.ScaleProgress = nil
		return Store(state)
	}
	// Unknown marker direction: clear it rather than wedge the app forever.
	state.ScaleProgress = nil
	return Store(state)
}

// stopUnrecordedReplicas stops live processes launched for an interrupted
// scale-up that the replica map never recorded. Identification is by the
// recorded launch PID from the marker plus a process scan for the app's
// replica binaries, mirroring DetectWorkerOrphans.
func stopUnrecordedReplicas(ctx context.Context, state *DeployState, marker *ScaleProgress) error {
	if marker.NewPID > 0 {
		binary := markerBinaryFor(state, marker)
		victim := &Instance{Slot: "scale", PID: marker.NewPID, BinaryPath: binary}
		stopInstance(ctx, victim, DefaultGracePeriod, 0)
	}
	known := make(map[int]bool, len(state.Replicas))
	exes := make(map[string]bool)
	for _, inst := range state.Replicas {
		if inst == nil {
			continue
		}
		if inst.PID > 0 {
			known[inst.PID] = true
		}
		if inst.BinaryPath != "" {
			exes[inst.BinaryPath] = true
		}
	}
	for _, pid := range scanBinaryPIDs(exes, known) {
		stopInstance(ctx, &Instance{Slot: "scale", PID: pid}, DefaultGracePeriod, 0)
	}
	return nil
}

// markerBinaryFor resolves the binary the interrupted scale-up launched from:
// the active version's artifact path.
func markerBinaryFor(state *DeployState, marker *ScaleProgress) string {
	for _, inst := range state.Replicas {
		if inst != nil && inst.BinaryPath != "" {
			return inst.BinaryPath
		}
	}
	if state.ActiveVersion > 0 {
		if binary, _, err := VersionPaths(state.AppName, state.ActiveVersion); err == nil {
			return binary
		}
	}
	return ""
}

// scanBinaryPIDs scans /proc for live processes running one of exes whose
// PID is not in exclude, the same discovery DetectWorkerOrphans performs.
func scanBinaryPIDs(exes map[string]bool, exclude map[int]bool) []int {
	if len(exes) == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || exclude[pid] {
			continue
		}
		exe, err := os.Readlink("/proc/" + entry.Name() + "/exe")
		if err != nil {
			continue
		}
		for want := range exes {
			if sameExecutable(exe, want) {
				pids = append(pids, pid)
				break
			}
		}
	}
	sort.Ints(pids)
	return pids
}

// persist writes the state with the standard store (or the test seam).
func (e *ScaleExecutor) persist(state *DeployState) error {
	if e.stateStore != nil {
		return e.stateStore(state)
	}
	return Store(state)
}

// clearProgress removes the scale marker from the state without writing.
func (e *ScaleExecutor) clearProgress(state *DeployState) {
	state.ScaleProgress = nil
}

func (e *ScaleExecutor) logger() Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return &nopLogger{}
}

func (e *ScaleExecutor) grace() time.Duration {
	if e.GracePeriod > 0 {
		return e.GracePeriod
	}
	return DefaultGracePeriod
}

func (e *ScaleExecutor) healthCfg() *health.DeployTierConfig {
	if e.HealthProvider != nil {
		return e.HealthProvider(e.AppID)
	}
	return nil
}

// resolveHealth resolves the deploy health tier the same way a rolling
// deploy does, so a scaled replica is gated by exactly the checks a
// deployed replica passed.
func (e *ScaleExecutor) resolveHealth(ctx context.Context, state *DeployState, log Logger) (health.Tier, *health.DeployTierConfig) {
	cfg := e.healthCfg()
	tier := selectDeployHealth(ctx, cfg, e.AppName, firstPortAddr(state), log, nil)
	return tier, cfg
}

func (e *ScaleExecutor) checkEnrolled() bool {
	if e.ProxyClient == nil {
		return false
	}
	st, err := e.ProxyClient.Status(context.Background(), e.AppName)
	return err == nil && len(st) > 0
}

func (e *ScaleExecutor) inFlight() int64 {
	if e.InFlight == nil {
		return 0
	}
	return e.InFlight(e.AppName)
}

// loadStateForScale loads the deployment state and settles any interrupted
// secret transaction first, the same way worker mutations do. (Internal
// helper shared by Scale.)
func loadStateForScale(appName string) (*DeployState, error) {
	state, err := Load(appName)
	if err != nil {
		return nil, err
	}
	if outcome, err := ReconcileSecretTxn(appName, state.AppID); err != nil {
		return nil, err
	} else if outcome != SecretTxnNone {
		return Load(appName)
	}
	return state, nil
}
