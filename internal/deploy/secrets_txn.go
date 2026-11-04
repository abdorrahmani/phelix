package deploy

import (
	"context"
	"errors"
	"os"
	"sync"

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
	ID           string
	DeploymentID string
	Set          *secrets.PinnedSet
}

// activeSecretTxns maps an application name to the transaction identifier
// whose window is open in this process. A window is open exactly while a
// deployment is executing, which the deploy lock guarantees cannot overlap
// with another mutation of the application. Store consults it so every
// durable write inside the window carries the stamp.
var activeSecretTxns = struct {
	mu sync.Mutex
	m  map[string]string
}{m: map[string]string{}}

// beginSecretTxnWindow opens the stamping window for an application.
func beginSecretTxnWindow(appName, txnID string) {
	activeSecretTxns.mu.Lock()
	defer activeSecretTxns.mu.Unlock()
	activeSecretTxns.m[appName] = txnID
}

// endSecretTxnWindow closes the stamping window. Later writes, including a
// recovery's, are never stamped with the ended transaction.
func endSecretTxnWindow(appName string) {
	activeSecretTxns.mu.Lock()
	defer activeSecretTxns.mu.Unlock()
	delete(activeSecretTxns.m, appName)
}

// stampedTxnID returns the identifier to stamp into a state write, or "".
func stampedTxnID(appName string) string {
	activeSecretTxns.mu.Lock()
	defer activeSecretTxns.mu.Unlock()
	return activeSecretTxns.m[appName]
}

// BeginSecretTxn pins the candidate bindings and records the write-ahead
// intent before anything about the deployment changes. The caller must hold
// the app's deploy lock. The intent record holds bindings and versions
// only; values stay in the returned context.
func BeginSecretTxn(appID, appName, deploymentID string, imports []secrets.Binding) (*SecretTxn, error) {
	if appID == "" || !appid.Valid(appID) {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument, "deploy: cannot open a secret transaction for app %q", appName)
	}
	txn := &SecretTxn{
		ID:           secrets.NewTxnID(),
		DeploymentID: deploymentID,
	}
	set, err := secrets.PinBindings(context.Background(), appID, imports)
	if err != nil {
		return nil, err
	}
	txn.Set = set
	pending := &secrets.PendingTxn{
		TxnID:        txn.ID,
		DeploymentID: deploymentID,
		Bindings:     set.Bindings,
		Resolved:     set.Resolved,
	}
	if err := secrets.BeginTxn(appID, pending); err != nil {
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

// endOnReturn opens the stamping window and returns the closer for a defer.
// Without a transaction it is a no-op.
func (t *SecretTxn) endOnReturn(appName string) func() {
	if t == nil {
		return func() {}
	}
	beginSecretTxnWindow(appName, t.ID)
	return func() { endSecretTxnWindow(appName) }
}

// FinishSecretTxn settles the transaction after the deployment returned.
// Success commits the candidate bindings; failure discards them. This runs
// after the engine's final durable write either way, and before any recovery
// the caller performs, so a rollback's journal writes are never stamped.
func FinishSecretTxn(appID string, txn *SecretTxn, deployErr error) error {
	if txn == nil {
		return nil
	}
	if deployErr != nil {
		return secrets.DiscardTxn(appID)
	}
	return secrets.CommitTxn(appID)
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
			if derr := secrets.DiscardTxn(appID); derr != nil {
				return SecretTxnRetained, derr
			}
			return SecretTxnDiscarded, nil
		}
		return SecretTxnRetained, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: read deployment journal of %s", appName)
	}
	if state.SecretTxnID == st.Pending.TxnID {
		if cerr := secrets.CommitTxn(appID); cerr != nil {
			return SecretTxnRetained, cerr
		}
		return SecretTxnCommitted, nil
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
