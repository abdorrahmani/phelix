package deploy

import (
	"context"
	"errors"
	"os"

	"github.com/abdorrahmani/phelix/internal/appid"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

// SecretTxn is a deployment's secret execution context. It exists from the
// moment a deployment pins its candidate bindings until the deployment
// engine's journal has settled. It carries the pinned values in memory and
// an identifier that the engine stamps into every durable acceptance write,
// so that after any crash the store can derive its outcome from the journal
// instead of deciding on its own.
type SecretTxn struct {
	AppName      string
	ID           string
	DeploymentID string
	Set          *secrets.PinnedSet
}

// BeginSecretTxn pins the candidate bindings and records the write-ahead
// intent before anything about the deployment changes. The caller must hold
// the app's deploy lock. The intent record holds bindings and versions
// only; values stay in the returned context.
func BeginSecretTxn(appID, appName, deploymentID string, imports []secrets.Binding) (*SecretTxn, error) {
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
func (t *SecretTxn) Stamp(state *DeployState) {
	state.SecretTxnID = ""
	if t != nil {
		state.SecretTxnID = t.ID
	}
}

// FinishSecretTxn settles bindings using the journal, including partial acceptance.
func FinishSecretTxn(appID string, txn *SecretTxn, deployErr error) error {
	if txn == nil {
		return nil
	}
	defer txn.Set.Close()
	_, err := ReconcileSecretTxn(txn.AppName, appID)
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
			if err := reclaimSecretLaunches(appName, st.Pending.TxnID, nil); err != nil {
				return SecretTxnRetained, err
			}
			if derr := secrets.DiscardTxn(appID); derr != nil {
				return SecretTxnRetained, derr
			}
			return SecretTxnDiscarded, nil
		}
		return SecretTxnRetained, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: read deployment journal of %s", appName)
	}
	if state.SecretTxnID == st.Pending.TxnID {
		if err := reclaimSecretLaunches(appName, st.Pending.TxnID, state); err != nil {
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
	if err := reclaimSecretLaunches(appName, st.Pending.TxnID, nil); err != nil {
		return SecretTxnRetained, err
	}
	if derr := secrets.DiscardTxn(appID); derr != nil {
		return SecretTxnRetained, derr
	}
	return SecretTxnDiscarded, nil
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
