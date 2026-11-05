package deploy

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

func TestSecretRecoveryChecksRealProcesses(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "discard"
		if accepted {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			txnTestEnv(t)
			seedSecret(t, "db", "candidate-credential")
			txn, err := BeginSecretTxn(txnTestAppID, name, "dep-1", []secrets.Binding{{Secret: "db", Env: "DB"}}, RuntimeNative)
			if err != nil {
				t.Fatal(err)
			}
			defer txn.Set.Close()
			cmd := exec.Command("sleep", "30")
			cmd.Env = append(os.Environ(), txn.Set.Overlay()...)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			state := &DeployState{AppName: name, AppID: txnTestAppID, Mode: ModeRolling}
			if accepted {
				state.Replicas = map[string]*Instance{"0": {PID: cmd.Process.Pid, Status: "running", SecretEnvs: []string{"DB"}}}
				txn.Stamp(state)
			}
			if err := Store(state); err != nil {
				t.Fatal(err)
			}
			outcome, err := ReconcileSecretTxn(name, txnTestAppID)
			if err != nil {
				t.Fatal(err)
			}
			st, err := secrets.Open(txnTestAppID)
			if err != nil || st.Pending != nil {
				t.Fatal("pending not settled", err)
			}
			if accepted {
				if outcome != SecretTxnCommitted {
					t.Fatal(outcome)
				}
				actual, err := secrets.RuntimeForPID(cmd.Process.Pid, []string{"DB"})
				if err != nil {
					t.Fatal(err)
				}
				defer actual.Close()
				desired, err := secrets.PinBindings(context.Background(), txnTestAppID, st.Bindings)
				if err != nil {
					t.Fatal(err)
				}
				defer desired.Close()
				if actual.Overlay()[0] != desired.Overlay()[0] {
					t.Fatal("runtime differs from recovered bindings")
				}
			} else {
				if outcome != SecretTxnDiscarded || len(st.Bindings) != 0 {
					t.Fatal("unaccepted bindings survived")
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("candidate was not terminated")
					}
				case <-time.After(time.Second):
					t.Fatal("unaccepted instance survived")
				}
			}
		})
	}
}

func TestAcceptedWorkerRecoveryDoesNotRevert(t *testing.T) {
	txnTestEnv(t)
	seedSecret(t, "db", "accepted-value")
	txn, err := BeginSecretTxn(txnTestAppID, "accepted-worker", "dep-1", []secrets.Binding{{Secret: "db", Env: "DB"}}, RuntimeNative)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Set.Close()
	state := &DeployState{AppName: txn.AppName, AppID: txnTestAppID, Mode: ModeWorker, WorkerRollout: &WorkerRollout{Status: WorkerRolloutRunning, ReplacedFrom: map[string]int{"0": 1}}}
	txn.Stamp(state)
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	if err := RecoverWorkerRollout(context.Background(), txn.AppName); err != nil {
		t.Fatal(err)
	}
	if mustLoad(t, txn.AppName).WorkerRollout.Status != WorkerRolloutDone {
		t.Fatal("worker recovery remained active")
	}
	st, _ := secrets.Open(txnTestAppID)
	if st.Pending != nil || len(st.Bindings) != 1 {
		t.Fatal("accepted bindings lost")
	}
}

func TestSecretDeployLockExcludesMutationCommand(t *testing.T) {
	txnTestEnv(t)
	release, err := AcquireDeployLock("serial-secret", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		release, err := AcquireDeployLock("serial-secret", "secrets-set")
		if err == nil {
			release()
		}
		done <- err
	}()
	if err := <-done; !phelixerr.IsCode(err, phelixerr.CodeDeployLocked) {
		t.Fatalf("mutation entered deployment: %v", err)
	}
	release()
	next, err := AcquireDeployLock("serial-secret", "secrets-set")
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestAcceptedCanaryRecoveryKeepsCandidate(t *testing.T) {
	txnTestEnv(t)
	stable := startChildInstance(t, SlotBlue, 1)
	candidate := startChildInstance(t, SlotGreen, 2)
	state := &DeployState{
		AppName: "accepted-canary", AppID: txnTestAppID, Mode: ModeBlueGreen,
		ActiveSlot: SlotBlue, ActiveVersion: 1, SecretTxnID: secrets.NewTxnID(),
		Slots:  map[string]*Instance{SlotBlue: &stable.Instance, SlotGreen: &candidate.Instance},
		Canary: &CanaryState{Slot: SlotGreen, Status: CanaryRunning, TrafficPercent: 25},
	}
	client := &rolloutProxyClient{alive: true, enrolled: true}
	if err := recoverAcceptedSecretCanary(state, client); err != nil {
		t.Fatal(err)
	}
	after := mustLoad(t, state.AppName)
	if after.ActiveSlot != SlotGreen || after.Canary.Status != CanaryPromoted || !InstanceAlive(after.ActiveInstance()) {
		t.Fatal("accepted canary was lost during recovery")
	}
	if InstanceAlive(&stable.Instance) {
		t.Fatal("retired stable instance survived recovery")
	}
	if targets := client.lastSwitchSet(); len(targets) != 1 || targets[0].Host != hostPort(candidate.Port) {
		t.Fatal("recovery did not route traffic to accepted candidate")
	}
}

func TestSecretRecoveryDrainsRecordedRetirements(t *testing.T) {
	for _, mode := range []Mode{ModeRolling, ModeBlueGreen, ModeWorker} {
		t.Run(string(mode), func(t *testing.T) {
			txnTestEnv(t)
			seedSecret(t, "db", "accepted-retirement-value")
			txn, err := BeginSecretTxn(txnTestAppID, "retirement", "dep-1", []secrets.Binding{{Secret: "db", Env: "DB"}}, RuntimeNative)
			if err != nil {
				t.Fatal(err)
			}
			defer txn.Set.Close()
			old := startChildInstance(t, "0", 1)
			current := startChildInstance(t, "0", 2)
			state := &DeployState{AppName: txn.AppName, AppID: txnTestAppID, Mode: mode,
				ActiveSlot: "0", Slots: map[string]*Instance{"0": &current.Instance},
				Replicas: map[string]*Instance{"0": &current.Instance}}
			txn.Stamp(state, &old.Instance, &current.Instance)
			if err := Store(state); err != nil {
				t.Fatal(err)
			}
			outcome, err := ReconcileSecretTxn(txn.AppName, txnTestAppID)
			if err != nil || outcome != SecretTxnCommitted {
				t.Fatalf("recovery: %s %v", outcome, err)
			}
			if InstanceAlive(&old.Instance) || !InstanceAlive(&current.Instance) {
				t.Fatal("recovery must stop retired process and keep accepted process")
			}
			if len(mustLoad(t, txn.AppName).Retiring) != 0 {
				t.Fatal("completed retirement remained in journal")
			}
			if outcome, err := ReconcileSecretTxn(txn.AppName, txnTestAppID); err != nil || outcome != SecretTxnNone {
				t.Fatal("recovery must be idempotent", outcome, err)
			}
		})
	}
}
