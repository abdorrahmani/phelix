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

// RestartSecretInstances applies each strategy while retaining the previous fleet for recovery.
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
	previous := *state
	previous.Replicas, previous.Slots = cloneInstanceMap(state.Replicas), cloneInstanceMap(state.Slots)
	previous.Retiring = append([]*Instance(nil), state.Retiring...)
	next := make(map[string]*Instance, len(instances))
	processes := make(map[string]Process, len(instances))
	var client *proxy.Client
	if state.Mode != ModeWorker {
		socket, err := proxy.DefaultSocketPath()
		if err != nil {
			return err
		}
		client = proxy.NewClient(socket)
	}
	changed := false
	defer func() {
		if errRet == nil {
			return
		}
		if changed {
			if client != nil {
				if err := RestoreProxyRoute(context.WithoutCancel(ctx), &previous, client, &nopLogger{}); err != nil {
					errRet = errors.Join(errRet, err)
					return
				}
			}
			for _, inst := range next {
				previous.Retiring = append(previous.Retiring, inst)
			}
			if err := Store(&previous); err != nil {
				errRet = errors.Join(errRet, err)
				return
			}
			*state = previous
		}
		for _, proc := range processes {
			stopHeldProcess(context.Background(), proc, 3*time.Second)
		}
	}()
	accept := func(records map[string]*Instance) error {
		for key, inst := range records {
			if state.Mode == ModeBlueGreen {
				state.Slots[key] = inst
			} else {
				state.Replicas[key] = inst
			}
			state.Retiring = append(state.Retiring, instances[key])
		}
		changed = true
		if client != nil {
			if err := RestoreProxyRoute(ctx, state, client, &nopLogger{}); err != nil {
				return err
			}
		}
		txn.Stamp(state)
		return Store(state)
	}
	instances = cloneInstanceMap(instances)
	rolling := state.Mode == ModeRolling || state.Mode == ModeWorker && worker.Strategy == workerStrategyRolling
	launcher := LauncherForRuntime(state.Runtime, state.Network, state.AppName)
	for _, key := range sortedReplicaKeys(instances) {
		if err := ctx.Err(); err != nil {
			return err
		}
		inst := instances[key]
		if inst == nil {
			continue
		}
		candidate := cloneInstance(inst)
		env, err := txn.OverlayFor(inst.EnvPath)
		if err != nil {
			return err
		}
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
			cfg := DefaultHealthProvider()(state.AppID)
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
		if rolling {
			if err := accept(map[string]*Instance{key: candidate}); err != nil {
				return err
			}
		}
	}
	if !rolling {
		if err := accept(next); err != nil {
			return err
		}
	}
	changed = false
	processes = nil
	var stopErrors []error
	for _, inst := range instances {
		if state.Mode == ModeWorker {
			_, _, err := GracefulStopWorker(inst, worker.signal(), worker.drain(), nil)
			stopErrors = append(stopErrors, err)
		} else {
			stopErrors = append(stopErrors, stopRetiredInstances(ctx, []*Instance{inst}, 3*time.Second, 0, &nopLogger{}))
		}
	}
	return errors.Join(stopErrors...)
}
