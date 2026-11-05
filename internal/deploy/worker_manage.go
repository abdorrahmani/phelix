package deploy

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Worker lifecycle commands shared by the CLI: start, stop, restart, scale and
// teardown. Every one takes the app's replica coordinator lock first, so the
// supervisor and the deploy engine never race them. Settings (signal, grace)
// are resolved by the caller from the project config.

// WorkerStart ensures every replica slot of a worker app is running: dead or
// missing replicas are launched at their recorded version. Slots already
// healthy are left alone. It returns the number of replicas launched.
func WorkerStart(ctx context.Context, appName string, sig os.Signal, grace time.Duration) (int, error) {
	release, err := (ReplicaCoordinator{}).LockApp(ctx, appName)
	if err != nil {
		return 0, err
	}
	defer release()

	state, err := loadWorkerForMutation(appName)
	if err != nil {
		return 0, err
	}
	if state.Mode != ModeWorker {
		return 0, phelixerr.Newf(phelixerr.CodeConfiguration, "%s is not a worker app (mode %s)", appName, state.Mode)
	}
	launched := 0
	for _, key := range replicaIndices(state.Replicas) {
		inst := state.Replicas[key]
		if inst == nil {
			continue
		}
		if WorkerInstanceAlive(inst) {
			if inst.Status != "running" && inst.Status != "starting" {
				inst.Status = "running"
				if inst.ReadyAt.IsZero() {
					inst.ReadyAt = time.Now()
				}
				_ = Store(state)
			}
			continue
		}
		proc, err := LaunchWorkerReplica(appName, state, key)
		if err != nil {
			return launched, phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "worker: start replica %s", key)
		}
		launched++
		if !waitForWorkerAlive(ctx, inst, proc, grace) {
			return launched, phelixerr.Newf(phelixerr.CodeInstanceStartFailed, "worker: replica %s exited immediately", key)
		}
		inst.Status = "running"
		inst.ReadyAt = time.Now()
		inst.RestartCount = 0
		_ = Store(state)
	}
	return launched, nil
}

// waitForWorkerAlive waits briefly for a freshly launched replica to still be
// alive, so an instantly-crashing binary fails the start command instead of
// reporting success.
func waitForWorkerAlive(ctx context.Context, inst *Instance, proc Process, within time.Duration) bool {
	deadline := time.Now().Add(within)
	if within <= 0 || within > 10*time.Second {
		within = 10 * time.Second
		deadline = time.Now().Add(within)
	}
	for {
		if !WorkerInstanceAlive(inst) {
			_ = proc.Wait()
			return false
		}
		select {
		case <-ctx.Done():
			return WorkerInstanceAlive(inst)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return WorkerInstanceAlive(inst)
		}
	}
}

// WorkerStop gracefully stops every replica of a worker app.
func WorkerStop(ctx context.Context, appName string, sig os.Signal, grace time.Duration) (int, error) {
	release, err := (ReplicaCoordinator{}).LockApp(ctx, appName)
	if err != nil {
		return 0, err
	}
	defer release()

	state, err := loadWorkerForMutation(appName)
	if err != nil {
		return 0, err
	}
	if state.Mode != ModeWorker {
		return 0, phelixerr.Newf(phelixerr.CodeConfiguration, "%s is not a worker app (mode %s)", appName, state.Mode)
	}
	stopped := 0
	for _, key := range replicaIndices(state.Replicas) {
		inst := state.Replicas[key]
		if inst == nil || !WorkerInstanceAlive(inst) {
			if inst != nil && inst.Status != "stopped" {
				inst.Status = "stopped"
				_ = Store(state)
			}
			continue
		}
		report, _, _ := GracefulStopWorker(inst, sig, grace, nil)
		inst.PID = 0
		inst.Status = "stopped"
		_ = Store(state)
		stopped++
		_ = report
	}
	return stopped, nil
}

// WorkerRestartReplica stops one replica, resets its crash counters and
// relaunches it at its recorded version.
func WorkerRestartReplica(ctx context.Context, appName, slot string, sig os.Signal, grace time.Duration) error {
	release, err := (ReplicaCoordinator{}).LockApp(ctx, appName)
	if err != nil {
		return err
	}
	defer release()

	state, err := loadWorkerForMutation(appName)
	if err != nil {
		return err
	}
	if state.Mode != ModeWorker {
		return phelixerr.Newf(phelixerr.CodeConfiguration, "%s is not a worker app (mode %s)", appName, state.Mode)
	}
	inst, ok := state.Replicas[slot]
	if !ok || inst == nil {
		return phelixerr.Newf(phelixerr.CodeNotFound, "worker %s has no replica %s", appName, slot)
	}
	if WorkerInstanceAlive(inst) {
		if _, _, err := GracefulStopWorker(inst, sig, grace, nil); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "worker: stop replica %s", slot)
		}
	}
	proc, err := LaunchWorkerReplica(appName, state, slot)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "worker: relaunch replica %s", slot)
	}
	inst.Status = "running"
	inst.ReadyAt = time.Now()
	inst.RestartCount = 0
	_ = Store(state)
	_ = proc
	return nil
}

// WorkerRestartAll stops and relaunches every replica at its current version,
// resetting crash counters.
func WorkerRestartAll(ctx context.Context, appName string, sig os.Signal, grace time.Duration) (int, error) {
	release, err := (ReplicaCoordinator{}).LockApp(ctx, appName)
	if err != nil {
		return 0, err
	}
	defer release()

	state, err := loadWorkerForMutation(appName)
	if err != nil {
		return 0, err
	}
	if state.Mode != ModeWorker {
		return 0, phelixerr.Newf(phelixerr.CodeConfiguration, "%s is not a worker app (mode %s)", appName, state.Mode)
	}
	count := 0
	for _, key := range replicaIndices(state.Replicas) {
		inst := state.Replicas[key]
		if inst == nil {
			continue
		}
		if WorkerInstanceAlive(inst) {
			if _, _, err := GracefulStopWorker(inst, sig, grace, nil); err != nil {
				return count, phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "worker: stop replica %s", key)
			}
		}
		if _, err := LaunchWorkerReplica(appName, state, key); err != nil {
			return count, phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "worker: relaunch replica %s", key)
		}
		inst.Status = "running"
		inst.ReadyAt = time.Now()
		inst.RestartCount = 0
		_ = Store(state)
		count++
	}
	return count, nil
}

// WorkerScale grows or shrinks the replica set at the current version without
// rebuilding. Growth launches the new slots; shrink drains the highest
// indices as intentional stops (they never count as crashes).
func WorkerScale(ctx context.Context, appName string, desired int, sig os.Signal, grace time.Duration) (from, to int, err error) {
	release, err := (ReplicaCoordinator{}).LockApp(ctx, appName)
	if err != nil {
		return 0, 0, err
	}
	defer release()

	state, err := loadWorkerForMutation(appName)
	if err != nil {
		return 0, 0, err
	}
	if state.Mode != ModeWorker {
		return 0, 0, phelixerr.Newf(phelixerr.CodeConfiguration, "%s is not a worker app (mode %s)", appName, state.Mode)
	}
	from = len(state.Replicas)
	if desired < 1 {
		return from, from, phelixerr.Newf(phelixerr.CodeInvalidArgument, "worker: --replicas must be >= 1, got %d", desired)
	}
	if desired == from {
		return from, from, nil
	}

	if desired > from {
		template := currentWorkerArtifact(state)
		if template.binaryPath == "" {
			return from, from, phelixerr.Newf(phelixerr.CodeConfiguration, "worker: no deployed version to scale; run a build or rebuild first")
		}
		state.Replicas = ensureReplicaMap(state.Replicas, desired)
		for i := from; i < desired; i++ {
			key := strconv.Itoa(i)
			inst := state.Replicas[key]
			inst.BinaryPath = template.binaryPath
			inst.EnvPath = template.envPath
			inst.Version = template.version
			if _, err := LaunchWorkerReplica(appName, state, key); err != nil {
				return from, i, phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "worker: launch replica %s", key)
			}
			inst.Status = "running"
			inst.ReadyAt = time.Now()
			_ = Store(state)
		}
		return from, desired, nil
	}

	surplus := applyReplicaSet(state.Replicas, desired)
	state.Replicas = ensureReplicaMap(state.Replicas, desired)
	for _, inst := range surplus {
		if inst == nil {
			continue
		}
		if WorkerInstanceAlive(inst) {
			if _, _, err := GracefulStopWorker(inst, sig, grace, nil); err != nil {
				return from, desired, phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "worker: drain replica %s", inst.Slot)
			}
		}
		inst.PID = 0
		inst.Status = "stopped"
	}
	if err := Store(state); err != nil {
		return from, desired, err
	}
	return from, desired, nil
}

// loadWorkerForMutation settles an interrupted deployment before changing replicas.
func loadWorkerForMutation(appName string) (*DeployState, error) {
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

// workerArtifact is the artifact triple a slot is launched from.
type workerArtifact struct {
	binaryPath string
	envPath    string
	version    int
}

// currentWorkerArtifact picks the freshest running replica's artifact as the
// template for new slots.
func currentWorkerArtifact(state *DeployState) workerArtifact {
	keys := replicaIndices(state.Replicas)
	for i := len(keys) - 1; i >= 0; i-- {
		inst := state.Replicas[keys[i]]
		if inst != nil && inst.BinaryPath != "" {
			return workerArtifact{binaryPath: inst.BinaryPath, envPath: inst.EnvPath, version: inst.Version}
		}
	}
	return workerArtifact{}
}

// WorkerReplicaSlots returns the sorted slot names of a worker app.
func WorkerReplicaSlots(appName string) ([]string, error) {
	state, err := Load(appName)
	if err != nil {
		return nil, err
	}
	if state.Mode != ModeWorker {
		return nil, phelixerr.Newf(phelixerr.CodeConfiguration, "%s is not a worker app", appName)
	}
	return replicaIndices(state.Replicas), nil
}

// WorkerSummary is the aggregated status of a worker app.
type WorkerSummary struct {
	Desired   int
	Running   int
	Failed    int // failed or crashloop replicas
	Stopped   int
	Crashloop bool
	Status    string // running | degraded | stopped
	// PID and StartedAt describe the lowest-slot alive replica (zero when
	// none), so readers can report live metrics and uptime.
	PID       int
	StartedAt time.Time
}

// SummarizeWorker inspects a worker app's replica records without probing.
func SummarizeWorker(state *DeployState) WorkerSummary {
	s := WorkerSummary{Desired: len(state.Replicas)}
	for _, inst := range state.Replicas {
		if inst == nil {
			continue
		}
		switch inst.Status {
		case "running", "starting":
			if WorkerInstanceAlive(inst) {
				s.Running++
			} else {
				s.Failed++
			}
		case "crashloop":
			s.Failed++
			s.Crashloop = true
		case "failed":
			s.Failed++
		default:
			s.Stopped++
		}
	}
	switch {
	case s.Desired == 0 || (s.Running == 0 && s.Failed == 0):
		s.Status = "stopped"
	case s.Running == s.Desired:
		s.Status = "running"
	default:
		s.Status = "degraded"
	}
	for _, key := range sortedIntSlots(state.Replicas) {
		inst := state.Replicas[key]
		if inst == nil || !WorkerInstanceAlive(inst) {
			continue
		}
		s.PID = inst.PID
		s.StartedAt = inst.StartedAt
		break
	}
	return s
}

// sortedIntSlots is sortedSlots for callers outside the supervisor.
func sortedIntSlots(m map[string]*Instance) []string {
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

// WorkerAlivePIDs returns every live replica PID in slot order, so callers
// that report per-app resource usage can aggregate the whole replica set.
func WorkerAlivePIDs(state *DeployState) []int {
	if state == nil {
		return nil
	}
	var pids []int
	for _, key := range sortedIntSlots(state.Replicas) {
		inst := state.Replicas[key]
		if inst != nil && WorkerInstanceAlive(inst) {
			pids = append(pids, inst.PID)
		}
	}
	return pids
}

// WorkerAppNames lists the apps whose persisted deploy state is ModeWorker,
// scanned from the app data directory. Read-only and manager-free, so the
// supervisor can call it from any goroutine.
func WorkerAppNames() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(home, ".phelix", "apps"))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		state, err := Load(entry.Name())
		if err != nil || state == nil || state.Mode != ModeWorker {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}
