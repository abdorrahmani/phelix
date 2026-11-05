package deploy

import (
	"context"
	"errors"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

// RestartSecretInstances proves replacements before retiring any running credentials.
func RestartSecretInstances(ctx context.Context, state *DeployState, desired *secrets.PinnedSet, worker *Worker, slot string) error {
	instances := state.Replicas
	if state.Mode == ModeBlueGreen {
		instances = map[string]*Instance{state.ActiveSlot: state.ActiveInstance()}
	}
	if slot != "" {
		if state.Mode != ModeWorker || instances[slot] == nil {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "requested worker replica does not exist")
		}
		instances = map[string]*Instance{slot: instances[slot]}
	}
	old := make(map[string]*Instance, len(instances))
	next := make(map[string]*Instance, len(instances))
	processes := make(map[string]Process, len(instances))
	cleanup := true
	defer func() {
		if cleanup {
			for _, proc := range processes {
				stopHeldProcess(context.Background(), proc, 3*time.Second)
			}
		}
	}()
	launcher := LauncherForRuntime(state.Runtime, state.Network, state.AppName)
	for _, key := range sortedReplicaKeys(instances) {
		inst := instances[key]
		if inst == nil {
			continue
		}
		old[key] = cloneInstance(inst)
		candidate := cloneInstance(inst)
		env, err := snapshotOverlay(inst.EnvPath, state.AppID)
		if err != nil {
			return err
		}
		env = mergeByKey(env, desired.Overlay())
		if state.Mode == ModeWorker {
			proc, id, err := workerLauncherFactory(state.AppName)(inst.BinaryPath, env, key)
			if err != nil {
				return err
			}
			processes[key] = proc
			candidate.PID, candidate.StartTicks = id.PID, id.StartTime
			if worker == nil {
				return phelixerr.New(phelixerr.CodeConfiguration, "worker restart settings are missing")
			}
			if err := worker.waitForReady(ctx, state, candidate, env); err != nil {
				return err
			}
		} else {
			proc, listenPort, err := launcher(ctx, inst.BinaryPath, env)
			if err != nil {
				return err
			}
			processes[key] = proc
			candidate.PID, candidate.Port, candidate.ContainerID = proc.PID(), listenPort, containerIDOf(proc)
			if !port.WaitForListener(hostPort(listenPort), 10*time.Second) {
				return phelixerr.New(phelixerr.CodeHealthCheckFailed, "replacement did not open its port; previous instances remain running")
			}
		}
		candidate.SecretEnvs = phelixerr.SecretEnvNames(env)
		candidate.Status, candidate.StartedAt, candidate.ReadyAt = "running", time.Now(), time.Now()
		next[key] = candidate
	}
	var client *proxy.Client
	if state.Mode != ModeWorker {
		socket, err := proxy.DefaultSocketPath()
		if err != nil {
			return err
		}
		client = proxy.NewClient(socket)
		primary, backends := healthyTargets(next)
		if err := client.Switch(ctx, state.AppName, primary, backends...); err != nil {
			return err
		}
	}
	put := func(records map[string]*Instance) {
		for key, inst := range records {
			if state.Mode == ModeBlueGreen {
				state.Slots[key] = inst
			} else {
				state.Replicas[key] = inst
			}
		}
	}
	put(next)
	stamp := state.SecretTxnID
	state.SecretTxnID = ""
	if err := Store(state); err != nil {
		if client != nil {
			primary, backends := healthyTargets(old)
			if restoreErr := client.Switch(context.WithoutCancel(ctx), state.AppName, primary, backends...); restoreErr != nil {
				cleanup = false
				return errors.Join(err, restoreErr)
			}
		}
		put(old)
		state.SecretTxnID = stamp
		return errors.Join(err, Store(state))
	}
	cleanup = false
	var stopErrors []error
	for _, inst := range old {
		if state.Mode == ModeWorker {
			_, _, err := GracefulStopWorker(inst, worker.signal(), worker.drain(), nil)
			stopErrors = append(stopErrors, err)
		} else {
			stopRetiredInstances(ctx, []*Instance{inst}, 3*time.Second, 0, &nopLogger{})
		}
	}
	return errors.Join(stopErrors...)
}
