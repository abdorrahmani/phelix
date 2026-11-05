package deploy

import (
	"context"
	"errors"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

// RestartSecretInstances proves replacements before retiring any running credentials.
func RestartSecretInstances(ctx context.Context, state *DeployState, desired *secrets.PinnedSet, worker *Worker, slot string) (errRet error) {
	if state.Mode == ModeWorker && worker == nil {
		return phelixerr.New(phelixerr.CodeConfiguration, "worker restart settings are missing")
	}
	if state.Mode != ModeWorker && state.Mode != ModeBlueGreen && state.Mode != ModeRolling {
		return phelixerr.New(phelixerr.CodeConfiguration, "unsupported deployment restart strategy")
	}
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
	txn := &SecretTxn{Partial: slot != "", AppName: state.AppName, ID: secrets.NewTxnID(), Set: desired}
	if err := secrets.BeginTxn(state.AppID, &secrets.PendingTxn{TxnID: txn.ID, Bindings: desired.Bindings, Resolved: desired.Resolved, Runtime: state.Runtime}); err != nil {
		return err
	}
	desired.LaunchTxnID = txn.ID
	defer func() { errRet = errors.Join(errRet, FinishSecretTxn(state.AppID, txn, errRet)) }()
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
		if err := ctx.Err(); err != nil {
			return err
		}
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
			candidate.StartTicks = 0
			if candidate.ContainerID == "" {
				identity, err := processex.Identity(candidate.PID)
				if err != nil {
					return phelixerr.New(phelixerr.CodeInstanceStartFailed, "cannot identify replacement process")
				}
				candidate.StartTicks = identity.StartTime
			}
			cfg := DefaultHealthProvider()(state.AppName)
			tier := selectDeployHealth(ctx, cfg, state.AppName, hostPort(listenPort), &nopLogger{}, nil)
			if err := health.WaitForHealthy(ctx, tier, cfg, hostPort(listenPort), proc.PID(), deployResolver(proc)); err != nil {
				return err
			}
		}
		candidate.SecretEnvs = phelixerr.SecretEnvNames(env)
		candidate.Status, candidate.StartedAt, candidate.ReadyAt = "running", time.Now(), time.Now()
		candidate.RestartCount = 0
		candidate.LastRestartAt = time.Now()
		candidate.LastExitAt = time.Time{}
		candidate.LastExitCode, candidate.LastExitReason = 0, ""
		next[key] = candidate
	}
	var client *proxy.Client
	if state.Mode != ModeWorker {
		socket, err := proxy.DefaultSocketPath()
		if err != nil {
			return err
		}
		client = proxy.NewClient(socket)
		route := *state
		if state.Mode == ModeBlueGreen {
			route.Slots = next
		} else {
			route.Replicas = next
		}
		if err := RestoreProxyRoute(ctx, &route, client, &nopLogger{}); err != nil {
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
	txn.Stamp(state)
	for _, inst := range old {
		state.Retiring = append(state.Retiring, inst)
	}
	cleanup = false
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
		for _, inst := range next {
			state.Retiring = append(state.Retiring, inst)
		}
		restoreErr := Store(state)
		cleanup = restoreErr == nil
		return errors.Join(err, restoreErr)
	}
	cleanup = false
	var stopErrors []error
	for _, inst := range old {
		if state.Mode == ModeWorker {
			_, _, err := GracefulStopWorker(inst, worker.signal(), worker.drain(), nil)
			stopErrors = append(stopErrors, err)
		} else {
			stopErrors = append(stopErrors, stopRetiredInstances(ctx, []*Instance{inst}, 3*time.Second, 0, &nopLogger{}))
		}
	}
	return errors.Join(stopErrors...)
}
