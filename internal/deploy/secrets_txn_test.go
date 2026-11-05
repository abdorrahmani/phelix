package deploy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

const txnTestAppID = "2f0e1d2c-3b4a-4958-8677-8899aabbccdd"

func txnTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func seedSecret(t *testing.T, name, value string) {
	t.Helper()
	if _, err := secrets.Set(txnTestAppID, name, value); err != nil {
		t.Fatalf("seed secret %s: %v", name, err)
	}
}

func TestStorePreservesOnlyExplicitAcceptanceStamp(t *testing.T) {
	txnTestEnv(t)
	state := &DeployState{AppName: "stamp-app", AppID: txnTestAppID, Mode: ModeRolling}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	if mustLoad(t, state.AppName).SecretTxnID != "" {
		t.Fatal("preparation stamped")
	}
	txn := &SecretTxn{ID: secrets.NewTxnID()}
	txn.Stamp(state)
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireDeployLock(state.AppName, "test")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if mustLoad(t, state.AppName).SecretTxnID != txn.ID {
		t.Fatal("lock erased acceptance evidence")
	}
	(*SecretTxn)(nil).Stamp(state)
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	if mustLoad(t, state.AppName).SecretTxnID != "" {
		t.Fatal("ordinary acceptance retained stamp")
	}
}

func TestBeginSecretTxnWritesDurableIntent(t *testing.T) {
	txnTestEnv(t)
	seedSecret(t, "db", "postgres://pinned-1")
	imports := []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}
	txn, err := BeginSecretTxn(txnTestAppID, "intent-app", "dep-1", imports, RuntimeNative)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	st, err := secrets.Open(txnTestAppID)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if st.Pending == nil || st.Pending.TxnID != txn.ID {
		t.Fatalf("pending = %+v, want txn %s", st.Pending, txn.ID)
	}
	if len(st.Pending.Resolved) != 1 || st.Pending.Resolved[0].Secret != "db" || st.Pending.Resolved[0].Version != 1 {
		t.Fatalf("resolved = %+v", st.Pending.Resolved)
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".phelix", "secrets", txnTestAppID+".json"))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(raw), "postgres://pinned-1") {
		t.Fatal("the durable intent record must never contain a value")
	}
}

func TestBeginSecretTxnMissingSecretLeavesNoIntent(t *testing.T) {
	txnTestEnv(t)
	imports := []secrets.Binding{{Secret: "ghost", Env: "GHOST"}}
	if _, err := BeginSecretTxn(txnTestAppID, "intent-app", "dep-1", imports, RuntimeNative); err == nil {
		t.Fatal("pinning must fail on a missing secret")
	}
	st, err := secrets.Open(txnTestAppID)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if st.Pending != nil {
		t.Fatalf("a failed pin must not record an intent, got %+v", st.Pending)
	}
}

func TestFinishSecretTxnCommitAndDiscard(t *testing.T) {
	t.Run("success commits the candidates", func(t *testing.T) {
		txnTestEnv(t)
		seedSecret(t, "db", "value-a")
		txn, err := BeginSecretTxn(txnTestAppID, "finish-app", "dep-1", []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}, RuntimeNative)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := Store(&DeployState{AppName: "finish-app", AppID: txnTestAppID, Mode: ModeRolling, SecretTxnID: txn.ID}); err != nil {
			t.Fatal(err)
		}
		if err := FinishSecretTxn(txnTestAppID, txn, nil); err != nil {
			t.Fatalf("finish: %v", err)
		}
		st, _ := secrets.Open(txnTestAppID)
		if st.Pending != nil || len(st.Bindings) != 1 || st.Bindings[0].Env != "DATABASE_URL" {
			t.Fatalf("after success: pending=%+v bindings=%+v", st.Pending, st.Bindings)
		}
	})
	t.Run("failure discards and keeps previous bindings", func(t *testing.T) {
		txnTestEnv(t)
		seedSecret(t, "old", "old-value")
		seedSecret(t, "new", "new-value")
		if err := secrets.ReplaceBindings(txnTestAppID, []secrets.Binding{{Secret: "old", Env: "OLD"}}); err != nil {
			t.Fatalf("seed bindings: %v", err)
		}
		txn, err := BeginSecretTxn(txnTestAppID, "finish-app", "dep-2", []secrets.Binding{{Secret: "new", Env: "NEW"}}, RuntimeNative)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := FinishSecretTxn(txnTestAppID, txn, phelixerr.New(phelixerr.CodeHealthCheckFailed, "health failed")); err != nil {
			t.Fatalf("finish: %v", err)
		}
		st, _ := secrets.Open(txnTestAppID)
		if st.Pending != nil || len(st.Bindings) != 1 || st.Bindings[0].Env != "OLD" {
			t.Fatalf("after failure: pending=%+v bindings=%+v", st.Pending, st.Bindings)
		}
	})
}

func TestReconcileSecretTxnCrashMatrix(t *testing.T) {
	t.Run("no pending record means nothing to do", func(t *testing.T) {
		txnTestEnv(t)
		outcome, err := ReconcileSecretTxn("reconcile-app", txnTestAppID)
		if err != nil || outcome != SecretTxnNone {
			t.Fatalf("outcome=%v err=%v", outcome, err)
		}
	})
	t.Run("unstamped journal discards", func(t *testing.T) {
		txnTestEnv(t)
		seedSecret(t, "db", "value")
		txn, err := BeginSecretTxn(txnTestAppID, "reconcile-app", "dep-1", []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}, RuntimeNative)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := Store(&DeployState{AppName: "reconcile-app", AppID: txnTestAppID, Mode: ModeRolling}); err != nil {
			t.Fatalf("store: %v", err)
		}
		_ = txn
		outcome, err := ReconcileSecretTxn("reconcile-app", txnTestAppID)
		if err != nil || outcome != SecretTxnDiscarded {
			t.Fatalf("outcome=%v err=%v", outcome, err)
		}
		st, _ := secrets.Open(txnTestAppID)
		if st.Pending != nil || len(st.Bindings) != 0 {
			t.Fatalf("discard must clear pending and keep old bindings: %+v %+v", st.Pending, st.Bindings)
		}
	})
	t.Run("stamped journal commits", func(t *testing.T) {
		txnTestEnv(t)
		seedSecret(t, "db", "value")
		txn, err := BeginSecretTxn(txnTestAppID, "reconcile-app", "dep-1", []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}, RuntimeNative)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := Store(&DeployState{AppName: "reconcile-app", AppID: txnTestAppID, Mode: ModeRolling, SecretTxnID: txn.ID}); err != nil {
			t.Fatalf("store: %v", err)
		}
		outcome, err := ReconcileSecretTxn("reconcile-app", txnTestAppID)
		if err != nil || outcome != SecretTxnCommitted {
			t.Fatalf("outcome=%v err=%v", outcome, err)
		}
		st, _ := secrets.Open(txnTestAppID)
		if st.Pending != nil || len(st.Bindings) != 1 || st.Bindings[0].Env != "DATABASE_URL" {
			t.Fatalf("commit must promote candidates: pending=%+v bindings=%+v", st.Pending, st.Bindings)
		}
	})
	t.Run("missing journal discards", func(t *testing.T) {
		txnTestEnv(t)
		seedSecret(t, "db", "value")
		if _, err := BeginSecretTxn(txnTestAppID, "nojournal-app", "dep-1", []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}, RuntimeNative); err != nil {
			t.Fatalf("begin: %v", err)
		}
		outcome, err := ReconcileSecretTxn("nojournal-app", txnTestAppID)
		if err != nil || outcome != SecretTxnDiscarded {
			t.Fatalf("outcome=%v err=%v", outcome, err)
		}
	})
	t.Run("unreadable journal retains and fails closed", func(t *testing.T) {
		txnTestEnv(t)
		seedSecret(t, "db", "value")
		if _, err := BeginSecretTxn(txnTestAppID, "corrupt-app", "dep-1", []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}, RuntimeNative); err != nil {
			t.Fatalf("begin: %v", err)
		}
		home := os.Getenv("HOME")
		stateDir := filepath.Join(home, ".phelix", "apps", "corrupt-app")
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "deploy.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("write corrupt journal: %v", err)
		}
		outcome, err := ReconcileSecretTxn("corrupt-app", txnTestAppID)
		if outcome != SecretTxnRetained || err == nil {
			t.Fatalf("an unreadable journal must retain the transaction, got outcome=%v err=%v", outcome, err)
		}
		st, _ := secrets.Open(txnTestAppID)
		if st.Pending == nil {
			t.Fatal("a retained transaction must stay pending")
		}
	})
}

func TestPendingDeploymentRejectsStoreRotation(t *testing.T) {
	txnTestEnv(t)
	seedSecret(t, "db", "pin-time-value")
	txn, err := BeginSecretTxn(txnTestAppID, "pin-app", "dep-1", []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}, RuntimeNative)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := secrets.Set(txnTestAppID, "db", "newer-value-during-deploy"); !phelixerr.IsCode(err, phelixerr.CodeDeployLocked) {
		t.Fatalf("rotation during deployment must fail: %v", err)
	}
	overlay, err := txn.OverlayFor("")
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}
	if len(overlay) != 2 || overlay[0] != "DATABASE_URL=pin-time-value" {
		t.Fatalf("every instance of a deployment must receive the pinned value, got %v", overlay)
	}
}

func TestRollingDeployUsesPinnedValuesUniformly(t *testing.T) {
	txnTestEnv(t)
	seedSecret(t, "db", "uniform-value-3")
	txn, err := BeginSecretTxn(txnTestAppID, "uniform-app", "dep-1", []secrets.Binding{{Secret: "db", Env: "DATABASE_URL"}}, RuntimeNative)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer FinishSecretTxn(txnTestAppID, txn, nil)

	var launched [][]string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	r := &Rolling{
		AppName:    "uniform-app",
		AppID:      txnTestAppID,
		PublicPort: port,
		Replicas:   2,
		Builder:    stubBuilder("/bin/true"),
		Launcher: func(_ context.Context, _ string, env []string) (Process, int, error) {
			launched = append(launched, append([]string(nil), env...))
			return &selfProc{pid: 1}, port, nil
		},
		ProxyClient:    &fakeProxyClient{alive: true},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		Logger:         &fakeLogger{},
		GracePeriod:    10 * time.Millisecond,
		Secrets:        txn,
	}
	if err := r.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(launched) != 2 {
		t.Fatalf("replicas launched = %d, want 2", len(launched))
	}
	for i, env := range launched {
		found := false
		for _, kv := range env {
			if kv == "DATABASE_URL=uniform-value-3" {
				found = true
			}
		}
		if !found {
			t.Fatalf("replica %d did not receive the pinned value: %v", i, env)
		}
	}
	state := mustLoad(t, "uniform-app")
	if state.SecretTxnID != txn.ID {
		t.Fatalf("journal stamp = %q, want %q", state.SecretTxnID, txn.ID)
	}
	st, _ := secrets.Open(txnTestAppID)
	if len(st.Bindings) != 0 {
		t.Fatalf("bindings must stay candidates until the deployment is finished, got %+v", st.Bindings)
	}
	if err := FinishSecretTxn(txnTestAppID, txn, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	st, _ = secrets.Open(txnTestAppID)
	if len(st.Bindings) != 1 || st.Bindings[0].Env != "DATABASE_URL" {
		t.Fatalf("bindings after finish = %+v", st.Bindings)
	}
}
