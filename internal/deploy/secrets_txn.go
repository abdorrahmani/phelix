package deploy

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/abdorrahmani/phelix/internal/appid"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

// SecretTxn is a deployment's secret execution context. It exists from the
// moment a deployment pins its candidate bindings until the deployment
// engine's journal has settled. It carries the pinned values in memory and
// an identifier that the engine stamps into every durable acceptance write,
// so that after any crash the store can derive its outcome from the journal
// instead of deciding on its own.
type SecretTxn struct {
	Partial      bool
	AppName      string
	ID           string
	DeploymentID string
	Set          *secrets.PinnedSet
}

// BeginSecretTxn pins the candidate bindings and records the write-ahead
// intent before anything about the deployment changes. The caller must hold
// the app's deploy lock. The intent record holds bindings and versions
// only; values stay in the returned context.
func BeginSecretTxn(appID, appName, deploymentID string, imports []secrets.Binding, runtime string) (*SecretTxn, error) {
	if appID == "" || !appid.Valid(appID) {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "deploy: cannot open a secret transaction for app %q", appName)
	}
	if _, err := ReconcileSecretTxn(appName, appID); err != nil {
		return nil, err
	}
	txn := &SecretTxn{
		AppName:      appName,
		ID:           secrets.NewTxnID(),
		DeploymentID: deploymentID,
	}
	set, err := secrets.PinBindings(context.Background(), appID, imports)
	if err != nil {
		return nil, err
	}
	txn.Set = set
	set.LaunchTxnID = txn.ID
	pending := &secrets.PendingTxn{
		Runtime:      runtime,
		TxnID:        txn.ID,
		DeploymentID: deploymentID,
		Bindings:     set.Bindings,
		Resolved:     set.Resolved,
	}
	if err := secrets.BeginTxn(appID, pending); err != nil {
		set.Close()
		return nil, err
	}
	return txn, nil
}

// OverlayFor builds the launch overlay for one instance of the deployment:
// the version's snapshot variables with the pinned secret values merged over
// them by name.
func (t *SecretTxn) OverlayFor(envPath string) ([]string, error) {
	base, err := snapshotOverlay(envPath, "")
	if err != nil {
		return nil, err
	}
	return mergeByKey(base, t.Set.Overlay()), nil
}

// Stamp marks a state that accepts instances using this deployment's values.
func (t *SecretTxn) Stamp(state *DeployState, retired ...*Instance) {
	state.secretPreparing = false
	state.SecretTxnID = ""
	if t != nil {
		state.SecretTxnID = t.ID
		for _, inst := range retired {
			if inst != nil && (inst.PID > 0 || inst.ContainerID != "") {
				state.Retiring = append(state.Retiring, cloneInstance(inst))
			}
		}
	}
}

// FinishSecretTxn settles bindings using the journal, including partial acceptance.
func FinishSecretTxn(appID string, txn *SecretTxn, deployErr error) error {
	if txn == nil {
		return nil
	}
	defer txn.Set.Close()
	outcome, err := ReconcileSecretTxn(txn.AppName, appID)
	if err == nil && outcome == SecretTxnCommitted && deployErr == nil {
		status := secrets.ApplyRestartSucceeded
		if txn.Partial {
			status = secrets.ApplyRestartPartial
		}
		return secrets.RecordApply(appID, &secrets.ApplyStatus{Restart: status})
	}
	return err
}

// SecretTxnOutcome describes what the reconciliation decided.
type SecretTxnOutcome string

const (
	// SecretTxnNone means the store holds no pending transaction.
	SecretTxnNone SecretTxnOutcome = "none"
	// SecretTxnCommitted means the journal showed the transaction was
	// accepted and the candidate bindings are now active.
	SecretTxnCommitted SecretTxnOutcome = "committed"
	// SecretTxnDiscarded means the journal showed the transaction was never
	// accepted and the previous bindings remain active.
	SecretTxnDiscarded SecretTxnOutcome = "discarded"
	// SecretTxnRetained means the journal could not be read, so the
	// transaction stays pending and the decision is deferred. Nothing is
	// committed on an unreadable journal.
	SecretTxnRetained SecretTxnOutcome = "retained"
)

// ReconcileSecretTxn derives the outcome of a pending secret transaction
// from the deployment engine's own journal. It is the crash-recovery rule
// and doubles as the manual reconciler behind `secrets apply`. The stamp the
// engine wrote into DeployState is the only success evidence the store
// accepts.
func ReconcileSecretTxn(appName, appID string) (SecretTxnOutcome, error) {
	if appID == "" || !appid.Valid(appID) {
		return SecretTxnNone, nil
	}
	st, err := secrets.Open(appID)
	if err != nil {
		return SecretTxnRetained, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: read secret store of %s", appName)
	}
	if st.Pending == nil {
		return SecretTxnNone, nil
	}
	state, err := Load(appName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := reclaimSecretLaunches(appName, st.Pending.TxnID, st.Pending.Runtime, nil); err != nil {
				return SecretTxnRetained, err
			}
			if derr := secrets.DiscardTxn(appID); derr != nil {
				return SecretTxnRetained, derr
			}
			return SecretTxnDiscarded, nil
		}
		return SecretTxnRetained, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: read deployment journal of %s", appName)
	}
	if state.AppName != appName || state.AppID != "" && state.AppID != appID || state.Mode != ModeRolling && state.Mode != ModeBlueGreen && state.Mode != ModeWorker && state.Mode != "classic" {
		return SecretTxnRetained, phelixerr.New(phelixerr.CodeConfiguration, "deployment journal identity or strategy is invalid; pending secrets retained")
	}
	if state.SecretTxnID == st.Pending.TxnID {
		if state.Canary != nil && state.Canary.Status == CanaryRunning {
			socket, err := proxy.DefaultSocketPath()
			if err != nil {
				return SecretTxnRetained, err
			}
			if err := recoverAcceptedSecretCanary(state, proxy.NewClient(socket)); err != nil {
				return SecretTxnRetained, err
			}
		}
		if err := reclaimSecretLaunches(appName, st.Pending.TxnID, state.Runtime, state); err != nil {
			return SecretTxnRetained, err
		}
		if err := drainSecretRetirements(state); err != nil {
			return SecretTxnRetained, err
		}
		if state.WorkerRollout != nil && state.WorkerRollout.Status != WorkerRolloutDone {
			state.WorkerRollout.Status = WorkerRolloutDone
			state.WorkerRollout.Reason = "accepted secret deployment recovered; later restarts use committed bindings"
			if err := Store(state); err != nil {
				return SecretTxnRetained, err
			}
		}
		if cerr := secrets.CommitTxn(appID); cerr != nil {
			return SecretTxnRetained, cerr
		}
		return SecretTxnCommitted, nil
	}
	runtime := st.Pending.Runtime
	if runtime == "" {
		runtime = state.Runtime
	}
	if err := reclaimSecretLaunches(appName, st.Pending.TxnID, runtime, state); err != nil {
		return SecretTxnRetained, err
	}
	if err := drainSecretRetirements(state); err != nil {
		return SecretTxnRetained, err
	}
	if derr := secrets.DiscardTxn(appID); derr != nil {
		return SecretTxnRetained, derr
	}
	return SecretTxnDiscarded, nil
}

// drainSecretRetirements finishes stops recorded with the replacement acceptance.
func drainSecretRetirements(state *DeployState) error {
	if len(state.Retiring) == 0 {
		return nil
	}
	serving := state.Replicas
	if state.Mode == ModeBlueGreen {
		serving = map[string]*Instance{state.ActiveSlot: state.ActiveInstance()}
		if state.Canary != nil && state.Canary.Status == CanaryRunning {
			serving[state.Canary.Slot] = state.Slots[state.Canary.Slot]
		}
	}
	for _, retired := range state.Retiring {
		if retired == nil {
			continue
		}
		inUse := false
		for _, inst := range serving {
			if inst != nil && ((retired.ContainerID != "" && retired.ContainerID == inst.ContainerID) || (retired.ContainerID == "" && retired.PID == inst.PID)) {
				inUse = true
			}
		}
		if inUse {
			continue
		}
		if state.Mode == ModeWorker {
			if _, _, err := GracefulStopWorker(retired, nil, 3*time.Second, nil); err != nil {
				return err
			}
		} else if report := stopInstance(context.Background(), retired, 3*time.Second, 0); !report.Exited {
			return phelixerr.New(phelixerr.CodeProcessFailed, "cannot stop retired secret instance")
		}
	}
	state.Retiring = nil
	return Store(state)
}

// recoverAcceptedSecretCanary preserves an accepted candidate across interrupted rollouts.
func recoverAcceptedSecretCanary(state *DeployState, client ProxyClient) error {
	canary := state.Canary
	candidate := state.Slots[canary.Slot]
	if candidate == nil {
		return phelixerr.New(phelixerr.CodeConfiguration, "accepted secret rollout has no candidate record")
	}
	if !InstanceAlive(candidate) {
		return phelixerr.New(phelixerr.CodeProcessFailed, "accepted secret rollout candidate is unavailable; pending secrets retained")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	promoted := *state
	promoted.ActiveSlot, promoted.ActiveVersion = canary.Slot, candidate.Version
	if err := RestoreProxyRoute(ctx, &promoted, client, &nopLogger{}); err != nil {
		return err
	}
	if err := Store(&promoted); err != nil {
		return err
	}
	*state = promoted
	for slot, inst := range state.Slots {
		if slot == canary.Slot || inst == nil {
			continue
		}
		if report := stopInstance(ctx, inst, 3*time.Second, 0); !report.Exited {
			return phelixerr.New(phelixerr.CodeProcessFailed, "accepted secret rollout could not stop its retired instance")
		}
		inst.PID, inst.ContainerID, inst.Status = 0, "", "stopped"
	}
	canary.Status, canary.TrafficPercent = CanaryPromoted, 100
	canary.Reason = "accepted secret rollout recovered from deployment journal"
	canary.UpdatedAt = time.Now()
	return Store(state)
}

// launchOverlay builds the environment overlay for a deployment's instance
// launcher. With an execution context it renders the pinned candidate set;
// without one it resolves the committed bindings as every non-deployment
// launch always has.
func launchOverlay(envPath, appID string, txn *SecretTxn) ([]string, error) {
	if txn != nil {
		return txn.OverlayFor(envPath)
	}
	return EnvOverlayFromSnapshot(envPath, appID)
}

// retireFailedCandidate records restored service before stopping a previously accepted candidate.
func retireFailedCandidate(state *DeployState, candidate *Instance, persist func(*DeployState) error) error {
	previous := cloneInstance(candidate)
	retiring := state.Retiring
	state.Retiring = append(append([]*Instance(nil), retiring...), previous)
	candidate.PID, candidate.ContainerID, candidate.Status = 0, "", "failed"
	state.secretPreparing = false
	if err := persist(state); err != nil {
		*candidate = *previous
		state.Retiring = retiring
		return err
	}
	return nil
}
