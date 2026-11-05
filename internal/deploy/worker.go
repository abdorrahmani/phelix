package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/secrets"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// WorkerGateExec selects the exec-probe startup gate (Worker.Health.Mode).
const WorkerGateExec = "exec"

// DefaultWorkerStartupTimeout bounds the startup gate when the caller did not
// resolve one from the project config.
const DefaultWorkerStartupTimeout = 30 * time.Second

// DefaultShutdownSignalName is the signal used when the caller did not
// resolve one from the project config.
const DefaultShutdownSignalName = "SIGTERM"

// Worker deploy strategies, mirroring the project.Strategy* values a
// phelix.yaml can declare for a worker.
const (
	workerStrategyClassic = "classic"
	workerStrategyRolling = "rolling"
)

// Worker deploys a type: worker app. There is no proxy and no port anywhere in
// this path: a replacement is proven by process identity plus the configured
// health gate, never by a listener. Two replacement policies exist:
//
//	classic  drain every replica, then launch the new version on all slots
//	rolling  replace one replica at a time; the old replica keeps running
//	         until its replacement passed the gate (surge 1, unavailable 0)
//
// The whole rollout holds the app's replica coordinator lock, so the
// supervisor never races it. Every per-replica transition is journaled in
// DeployState.WorkerRollout; on failure the engine reverts exactly the
// replicas it replaced (the v1 recovery contract), and an interrupted rollout
// is recovered by the next Deploy or the monitor.
type Worker struct {
	AppName string
	AppID   string
	// Source supplies the new version's binary and env snapshot.
	Source BuildSource
	// Strategy is StrategyClassic or StrategyRolling.
	Strategy string
	// Secrets carries the deployment's secret execution context. Nil means
	// the deployment resolves committed bindings (or none) at launch as
	// before.
	Secrets         *SecretTxn
	SecretImports   []secrets.Binding
	previousSecrets map[string]*secrets.PinnedSet
	Replicas        int
	// Dir is the app project directory: the exec probe's working directory.
	Dir string
	// Signal and grace govern replica shutdown; DrainTimeout bounds the wait
	// when retiring a replaced replica (0 falls back to the grace period).
	Signal         os.Signal
	Grace          time.Duration
	DrainTimeout   time.Duration
	StartupTimeout time.Duration
	// Health configures the startup gate: mode "process" (identity liveness)
	// or "exec" (probe command with Timeout).
	Health    WorkerGate
	Telemetry *Tracker
	Logger    Logger
}

// WorkerGate is the worker startup-gate configuration.
type WorkerGate struct {
	Mode    string
	Command []string
	Timeout time.Duration
}

func (w *Worker) logger() Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return &nopLogger{}
}

func (w *Worker) signal() os.Signal {
	if w.Signal != nil {
		return w.Signal
	}
	sig, err := SignalFromName(DefaultShutdownSignalName)
	if err != nil {
		return nil
	}
	return sig
}

func (w *Worker) drain() time.Duration {
	if w.DrainTimeout > 0 {
		return w.DrainTimeout
	}
	return w.grace()
}

func (w *Worker) grace() time.Duration {
	if w.Grace > 0 {
		return w.Grace
	}
	return DefaultGracePeriod
}

func (w *Worker) startupTimeout() time.Duration {
	if w.StartupTimeout > 0 {
		return w.StartupTimeout
	}
	return DefaultWorkerStartupTimeout
}

// Deploy runs the rollout. On failure the replaced replicas are reverted
// before the error returns: the returned error always describes the fleet
// state truthfully.
func (w *Worker) Deploy(ctx context.Context) (errRet error) {
	defer func() { reportDeployFailure(w.Telemetry, errRet) }()
	if w.Replicas < 1 {
		w.Replicas = 1
	}
	log := w.logger()

	release, err := (ReplicaCoordinator{}).LockApp(ctx, w.AppName)
	if err != nil {
		return err
	}
	defer release()

	if _, err := ReconcileSecretTxn(w.AppName, w.AppID); err != nil {
		return err
	}
	state, err := LoadOrInit(w.AppName, ModeWorker, 0)
	if err != nil {
		return wrapDeployStep(err, "load worker deploy state")
	}
	if state.AppID == "" {
		state.AppID = w.AppID
	}
	w.Telemetry.Bind(state)
	w.Telemetry.SetReplicasDesired(w.Replicas)
	w.Telemetry.Started(activeVersionOf(state, w.AppName), 0,
		fmt.Sprintf("worker deploy of %s over %d replicas (%s)", w.AppName, w.Replicas, w.Strategy))

	// An interrupted or half-reverted predecessor heals first: v1 recovery
	// always reverts to the journal's recorded previous versions.
	if state.WorkerRollout != nil && state.WorkerRollout.Status != WorkerRolloutDone {
		log.Stepf("recovering interrupted worker rollout (status %s, target v%d)",
			state.WorkerRollout.Status, state.WorkerRollout.Version)
		if rerr := w.revertRollout(ctx, state, state.WorkerRollout); rerr != nil {
			return wrapDeployStep(rerr, "recover interrupted worker rollout")
		}
		state.WorkerRollout = nil
	}

	if w.SecretImports != nil {
		w.Secrets, err = BeginSecretTxn(w.AppID, w.AppName, w.Telemetry.DeploymentID(), w.SecretImports)
		if err != nil {
			return err
		}
		defer func() { errRet = errors.Join(errRet, FinishSecretTxn(w.AppID, w.Secrets, errRet)) }()
	}

	if w.Secrets != nil {
		w.previousSecrets = map[string]*secrets.PinnedSet{}
		defer func() {
			for _, set := range w.previousSecrets {
				set.Close()
			}
		}()
		bindings, err := secrets.CommittedBindings(w.AppID)
		if err != nil {
			return err
		}
		for key, inst := range state.Replicas {
			if inst != nil && WorkerInstanceAlive(inst) {
				set, err := RuntimeSecrets(inst, bindings)
				if err != nil {
					return err
				}
				w.previousSecrets[key] = set
			}
		}
	}

	ReapStaleInstances(state)
	surplus := applyReplicaSet(state.Replicas, w.Replicas)
	state.Replicas = ensureReplicaMap(state.Replicas, w.Replicas)
	w.stopSurplus(ctx, surplus)

	w.Telemetry.Building(w.Source.Describe())
	binaryPath, envPath, err := w.Source.Build(ctx)
	if err != nil {
		return wrapDeployStep(err, "worker build")
	}
	targetVer := targetVersionFromSource(w.Source)
	w.Telemetry.SetTargetVersion(targetVer)

	journal := &WorkerRollout{
		Version:      targetVer,
		Strategy:     w.Strategy,
		Status:       WorkerRolloutRunning,
		ReplacedFrom: map[string]int{},
		StartedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	state.WorkerRollout = journal
	if err := Store(state); err != nil {
		return wrapDeployStep(err, "persist worker rollout journal")
	}

	env, err := launchOverlay(envPath, state.AppID, w.Secrets)
	if err != nil {
		return wrapDeployStep(err, "decrypt worker env snapshot")
	}

	if w.Strategy == workerStrategyRolling {
		err = w.deployRolling(ctx, state, binaryPath, envPath, env, targetVer, journal)
	} else {
		err = w.deployClassic(ctx, state, binaryPath, envPath, env, targetVer, journal)
	}
	if err != nil {
		if rerr := w.revertRollout(ctx, state, journal); rerr != nil {
			return phelixerr.Wrap(phelixerr.CodeDeployFailed,
				"worker deploy failed and recovery also failed; the rollout is marked recovery_required", errors.Join(err, rerr))
		}
		return err
	}

	journal.Status = WorkerRolloutDone
	journal.UpdatedAt = time.Now()
	if targetVer > 0 {
		state.ActiveVersion = targetVer
		if err := PromoteVersion(w.AppName, targetVer, "worker"); err != nil {
			return err
		}
	}
	if err := Store(state); err != nil {
		return wrapDeployStep(err, "persist worker deploy state")
	}
	if err := FinishSecretTxn(w.AppID, w.Secrets, nil); err != nil {
		return err
	}
	w.Secrets = nil
	w.Telemetry.PromoteCurrentVersion()
	w.Telemetry.Completed(fmt.Sprintf("%d worker replicas on v%d", w.Replicas, targetVer))
	return nil
}

// deployRolling replaces replicas one at a time: launch replacement, prove it,
// then drain the old replica. A failure aborts before the next replica.
func (w *Worker) deployRolling(ctx context.Context, state *DeployState, binaryPath, envPath string, env []string, targetVer int, journal *WorkerRollout) error {
	log := w.logger()
	for _, key := range replicaIndices(state.Replicas) {
		old := cloneInstance(state.Replicas[key])
		if old != nil && w.Secrets == nil && old.Version == targetVer && old.Status == "running" && WorkerInstanceAlive(old) {
			log.Stepf("replica %s already runs v%d; skipping", key, targetVer)
			continue
		}
		log.Stepf("replacing worker replica %s (v%d -> v%d)", key, versionOf(old), targetVer)
		if err := w.replaceOne(ctx, state, key, binaryPath, envPath, env, targetVer, journal, old); err != nil {
			return err
		}
	}
	return nil
}

// replaceOne swaps one replica: the slot's record is pointed at the new
// version and the replacement is launched alongside the old replica, proven
// by the startup gate, and only then is the old replica drained.
func (w *Worker) replaceOne(ctx context.Context, state *DeployState, key, binaryPath, envPath string, env []string, targetVer int, journal *WorkerRollout, old *Instance) error {
	log := w.logger()
	index, _ := strconv.Atoi(key)
	oldAlive := old != nil && WorkerInstanceAlive(old)

	inst := state.Replicas[key]
	inst.BinaryPath = binaryPath
	inst.EnvPath = envPath
	inst.Version = targetVer
	proc, err := launchWorkerReplica(w.AppName, state, key, w.Secrets)
	if err != nil {
		restoreReplica(state, key, old)
		_ = Store(state)
		return wrapDeployStep(err, "launch worker replica "+key)
	}
	w.Telemetry.ReplicaStarted(index, inst.PID, 0)
	w.Telemetry.ReplicaHealthCheckStarted(index, 0)

	if err := w.waitForReady(ctx, state, inst, env); err != nil {
		// Unproven replacement: stop it and put the old record back. The old
		// replica was never signalled and keeps serving.
		_, _, _ = GracefulStopWorker(inst, w.signal(), w.grace(), proc)
		restoreReplica(state, key, old)
		_ = Store(state)
		return wrapDeployStep(err, "worker replica "+key+" failed its startup gate")
	}
	inst.Status = "running"
	inst.ReadyAt = time.Now()
	oldStamp := state.SecretTxnID
	journal.ReplacedFrom[key] = versionOf(old)
	journal.UpdatedAt = time.Now()
	w.Secrets.Stamp(state)
	if err := Store(state); err != nil {
		_, _, _ = GracefulStopWorker(inst, w.signal(), w.grace(), proc)
		restoreReplica(state, key, old)
		state.SecretTxnID = oldStamp
		return err
	}
	w.Telemetry.ReplicaHealthy(index, inst.PID, 0)

	if oldAlive {
		w.Telemetry.ReplicaDraining(index, old.PID)
		report, _, _ := GracefulStopWorker(old, w.signal(), w.drain(), nil)
		if report.ForceKilled {
			log.Warnf("old replica %s ignored the shutdown signal; SIGKILL applied", key)
		}
		w.Telemetry.ReplicaStopped(index, old.PID, drainOutcome(report))
	}
	w.Telemetry.ReplicaReplaced(index, inst.PID, 0, fmt.Sprintf("replica %s on v%d", key, targetVer))
	return nil
}

// deployClassic drains every replica first, then launches the new version on
// all slots. Simpler than rolling, and the default worker strategy.
func (w *Worker) deployClassic(ctx context.Context, state *DeployState, binaryPath, envPath string, env []string, targetVer int, journal *WorkerRollout) error {
	log := w.logger()
	for _, key := range replicaIndices(state.Replicas) {
		inst := state.Replicas[key]
		if inst == nil {
			continue
		}
		journal.ReplacedFrom[key] = inst.Version
		if err := Store(state); err != nil {
			return err
		}
		if !WorkerInstanceAlive(inst) {
			inst.PID = 0
			inst.Status = "stopped"
			continue
		}
		index, _ := strconv.Atoi(key)
		w.Telemetry.ReplicaDraining(index, inst.PID)
		report, _, _ := GracefulStopWorker(inst, w.signal(), w.drain(), nil)
		if report.ForceKilled {
			log.Warnf("replica %s ignored the shutdown signal; SIGKILL applied", key)
		}
		inst.PID = 0
		inst.Status = "stopped"
		w.Telemetry.ReplicaStopped(index, inst.PID, drainOutcome(report))
		journal.UpdatedAt = time.Now()
		_ = Store(state)
	}
	for _, key := range replicaIndices(state.Replicas) {
		inst := state.Replicas[key]
		if inst == nil {
			continue
		}
		inst.BinaryPath = binaryPath
		inst.EnvPath = envPath
		inst.Version = targetVer
		proc, err := launchWorkerReplica(w.AppName, state, key, w.Secrets)
		if err != nil {
			return wrapDeployStep(err, "launch worker replica "+key)
		}
		index, _ := strconv.Atoi(key)
		w.Telemetry.ReplicaStarted(index, inst.PID, 0)
		w.Telemetry.ReplicaHealthCheckStarted(index, 0)
		if err := w.waitForReady(ctx, state, inst, env); err != nil {
			_, _, _ = GracefulStopWorker(inst, w.signal(), w.grace(), proc)
			inst.PID = 0
			inst.Status = "stopped"
			_ = Store(state)
			return wrapDeployStep(err, "worker replica "+key+" failed its startup gate")
		}
		inst.Status = "running"
		inst.ReadyAt = time.Now()
		journal.UpdatedAt = time.Now()
		w.Secrets.Stamp(state)
		if err := Store(state); err != nil {
			return err
		}
		w.Telemetry.ReplicaHealthy(index, inst.PID, 0)
	}
	return nil
}

// waitForReady proves a replacement: identity-verified liveness, and for exec
// mode a passing probe, within the startup window.
func (w *Worker) waitForReady(ctx context.Context, state *DeployState, inst *Instance, env []string) error {
	if w.Health.Mode != WorkerGateExec {
		if !WorkerInstanceAlive(inst) {
			return phelixerr.Newf(phelixerr.CodeInstanceStartFailed,
				"worker replica %s exited during startup", inst.Slot)
		}
		return nil
	}
	deadline := time.Now().Add(w.startupTimeout())
	var lastDetail string
	for {
		if !WorkerInstanceAlive(inst) {
			return phelixerr.Newf(phelixerr.CodeInstanceStartFailed,
				"worker replica %s exited during startup%s", inst.Slot, workerTail(inst))
		}
		outcome := RunProbe(w.Health.Command, w.Dir, env, w.Health.Timeout)
		if outcome.OK {
			return nil
		}
		lastDetail = outcome.Detail
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return phelixerr.Newf(phelixerr.CodeHealthCheckFailed,
				"worker replica %s did not become healthy within %s: %s",
				inst.Slot, w.startupTimeout(), lastDetail)
		}
	}
}

// revertRollout implements the v1 recovery contract: every replica the
// rollout replaced goes back to the version recorded in the journal. One
// failed revert flips the journal to recovery_required and the error
// propagates; the deploy never claims the fleet is back on the old version
// when it is not.
func (w *Worker) revertRollout(ctx context.Context, state *DeployState, journal *WorkerRollout) error {
	log := w.logger()
	if journal == nil || len(journal.ReplacedFrom) == 0 {
		if journal != nil {
			journal.Status = WorkerRolloutDone
			journal.Reason = "deploy failed before any replica was replaced"
			journal.UpdatedAt = time.Now()
			if err := Store(state); err != nil {
				return err
			}
		}
		return nil
	}
	journal.Status = WorkerRolloutReverting
	journal.UpdatedAt = time.Now()
	if err := Store(state); err != nil {
		return err
	}

	for _, key := range sortedJournalKeys(journal.ReplacedFrom) {
		fromVer := journal.ReplacedFrom[key]
		inst := state.Replicas[key]
		if inst != nil && inst.Version == fromVer && WorkerInstanceAlive(inst) && w.Secrets == nil {
			continue
		}
		if fromVer <= 0 {
			// The slot had no version before: leave it stopped.
			if inst != nil && WorkerInstanceAlive(inst) {
				if _, _, err := GracefulStopWorker(inst, w.signal(), w.drain(), nil); err != nil {
					return err
				}
			}
			if inst != nil {
				inst.PID = 0
				inst.Status = "stopped"
				if err := Store(state); err != nil {
					return err
				}
			}
			continue
		}
		bin, envPath, err := VersionPaths(w.AppName, fromVer)
		if err != nil {
			return w.recoveryRequired(state, journal, fmt.Sprintf("replica %s: resolve v%d: %v", key, fromVer, err))
		}
		if inst != nil && WorkerInstanceAlive(inst) {
			if _, _, err := GracefulStopWorker(inst, w.signal(), w.drain(), nil); err != nil {
				return err
			}
			inst.PID = 0
		}
		if inst == nil {
			inst = &Instance{Slot: key}
			state.Replicas[key] = inst
		}
		inst.BinaryPath = bin
		inst.EnvPath = envPath
		inst.Version = fromVer
		var previous *SecretTxn
		if set := w.previousSecrets[key]; set != nil {
			previous = &SecretTxn{Set: set}
		}
		proc, err := launchWorkerReplica(w.AppName, state, key, previous)
		if err != nil {
			return w.recoveryRequired(state, journal, fmt.Sprintf("replica %s: relaunch v%d: %v", key, fromVer, err))
		}
		env, err := launchOverlay(envPath, state.AppID, previous)
		if err != nil {
			return err
		}
		if err := w.waitForReady(ctx, state, inst, env); err != nil {
			_, _, _ = GracefulStopWorker(inst, w.signal(), w.grace(), proc)
			inst.PID = 0
			inst.Status = "stopped"
			if err := Store(state); err != nil {
				return err
			}
			return w.recoveryRequired(state, journal, fmt.Sprintf("replica %s: reverted v%d failed its gate: %v", key, fromVer, err))
		}
		inst.Status = "running"
		inst.ReadyAt = time.Now()
		journal.UpdatedAt = time.Now()
		state.SecretTxnID = ""
		if err := Store(state); err != nil {
			return err
		}
		log.Stepf("replica %s reverted to v%d", key, fromVer)
	}
	journal.Status = WorkerRolloutDone
	state.SecretTxnID = ""
	journal.Reason = "reverted to the previous versions after a failed deploy"
	journal.ReplacedFrom = nil
	journal.UpdatedAt = time.Now()
	return Store(state)
}

// recoveryRequired persists the honest terminal state for a rollout whose
// revert could not complete.
func (w *Worker) recoveryRequired(state *DeployState, journal *WorkerRollout, reason string) error {
	journal.Status = WorkerRolloutRecoveryRequired
	journal.Reason = reason
	journal.UpdatedAt = time.Now()
	if err := Store(state); err != nil {
		return err
	}
	return phelixerr.Newf(phelixerr.CodeDeployFailed,
		"worker rollout recovery failed: %s (recovery_required persisted)", reason)
}

// stopSurplus drains replicas removed by a shrink.
func (w *Worker) stopSurplus(ctx context.Context, surplus []*Instance) {
	for _, inst := range surplus {
		if inst == nil || inst.PID <= 0 {
			continue
		}
		if !WorkerInstanceAlive(inst) {
			inst.PID = 0
			inst.Status = "stopped"
			continue
		}
		w.logger().Stepf("stopping surplus worker replica %s (pid %d)", inst.Slot, inst.PID)
		report, _, _ := GracefulStopWorker(inst, w.signal(), w.drain(), nil)
		inst.PID = 0
		inst.Status = "stopped"
		w.Telemetry.ReplicaStopped(replicaIndexOf(inst.Slot), inst.PID, drainOutcome(report))
	}
}

func versionOf(inst *Instance) int {
	if inst == nil {
		return 0
	}
	return inst.Version
}

func replicaIndexOf(slot string) int {
	n, err := strconv.Atoi(slot)
	if err != nil {
		return 0
	}
	return n
}

func sortedJournalKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ai, _ := strconv.Atoi(keys[i])
		aj, _ := strconv.Atoi(keys[j])
		return ai < aj
	})
	return keys
}

// workerTail appends the replica's log tail to a startup failure so the
// actual panic line surfaces, mirroring the instance log tail for services.
func workerTail(inst *Instance) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := workerLogPath(home, inst.BinaryPath, inst.Slot)
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	const maxBytes = 2048
	tail := phelixerr.Redact(string(raw))
	if len(tail) > maxBytes {
		tail = tail[len(tail)-maxBytes:]
	}
	return "; output: " + phelixerr.Redact(strings.TrimSpace(tail))
}

// RecoverWorkerRollout heals an interrupted or half-reverted worker rollout
// after a daemon restart: v1 recovery always reverts to the journal's
// recorded previous versions. It is a no-op when no rollout needs recovery.
func RecoverWorkerRollout(ctx context.Context, appName string) error {
	release, err := (ReplicaCoordinator{}).LockApp(ctx, appName)
	if err != nil {
		return err
	}
	defer release()
	state, err := Load(appName)
	if err != nil || state == nil || state.Mode != ModeWorker {
		return err
	}
	if _, err := ReconcileSecretTxn(appName, state.AppID); err != nil {
		return err
	}
	state, err = Load(appName)
	if err != nil {
		return err
	}
	if state.WorkerRollout == nil || state.WorkerRollout.Status == WorkerRolloutDone {
		return nil
	}
	w := &Worker{AppName: appName, AppID: state.AppID}
	return w.revertRollout(ctx, state, state.WorkerRollout)
}
