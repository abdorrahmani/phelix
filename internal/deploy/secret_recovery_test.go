package deploy

import (
	"context"
	"errors"
	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/resources"
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

func TestSecretMigrationLeavesServingJournalUntilAcceptance(t *testing.T) {
	txnTestEnv(t)
	seedSecret(t, "db", "migration-credential")
	stable := startChildInstance(t, SlotBlue, 1)
	state := &DeployState{AppName: "migration-crash", AppID: txnTestAppID, Mode: ModeBlueGreen, ActiveSlot: SlotBlue, Slots: map[string]*Instance{SlotBlue: &stable.Instance}}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	txn, err := BeginSecretTxn(txnTestAppID, state.AppName, "dep-1", []secrets.Binding{{Secret: "db", Env: "DB"}}, RuntimeNative)
	if err != nil {
		t.Fatal(err)
	}
	defer txn.Set.Close()
	state.secretPreparing = true
	if _, err := state.MigrateTo(ModeRolling); err != nil {
		t.Fatal(err)
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	if recovered := mustLoad(t, state.AppName); recovered.Mode != ModeBlueGreen || !InstanceAlive(recovered.ActiveInstance()) {
		t.Fatal("preparation displaced serving journal before acceptance")
	}
	if outcome, err := ReconcileSecretTxn(state.AppName, txnTestAppID); err != nil || outcome != SecretTxnDiscarded {
		t.Fatal("unaccepted migration did not discard", outcome, err)
	}
}

func TestAcceptedCanaryRecoveryRetainsJournalOnUnavailableTarget(t *testing.T) {
	for _, dead := range []bool{false, true} {
		t.Run(map[bool]string{false: "proxy", true: "candidate"}[dead], func(t *testing.T) {
			txnTestEnv(t)
			stable := startChildInstance(t, SlotBlue, 1)
			candidate := startChildInstance(t, SlotGreen, 2)
			if dead {
				stopInstance(context.Background(), &candidate.Instance, time.Second, 0)
			}
			state := &DeployState{AppName: "retained-canary", AppID: txnTestAppID, Mode: ModeBlueGreen,
				ActiveSlot: SlotBlue, Slots: map[string]*Instance{SlotBlue: &stable.Instance, SlotGreen: &candidate.Instance},
				Canary: &CanaryState{Slot: SlotGreen, Status: CanaryRunning}}
			if err := Store(state); err != nil {
				t.Fatal(err)
			}
			client := &rolloutProxyClient{alive: true, enrolled: true, failSwitch: !dead}
			if err := recoverAcceptedSecretCanary(state, client); err == nil {
				t.Fatal("unavailable target was promoted")
			}
			if got := mustLoad(t, state.AppName); got.ActiveSlot != SlotBlue || got.Canary.Status != CanaryRunning || !InstanceAlive(&stable.Instance) {
				t.Fatal("failed recovery changed serving journal or stopped stable instance")
			}
		})
	}
}

func TestWorkerCompensationAcceptsRestoredFleetTogether(t *testing.T) {
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "interrupted", false: "completed"}[fail], func(t *testing.T) {
			txnTestEnv(t)
			seedSecret(t, "old", "previous-compensation-value")
			seedSecret(t, "next", "candidate-compensation-value")
			oldBindings := []secrets.Binding{{Secret: "old", Env: "DB"}}
			if err := secrets.ReplaceBindings(txnTestAppID, oldBindings); err != nil {
				t.Fatal(err)
			}
			txn, err := BeginSecretTxn(txnTestAppID, "compensation", "dep", []secrets.Binding{{Secret: "next", Env: "DB"}}, RuntimeNative)
			if err != nil {
				t.Fatal(err)
			}
			defer txn.Set.Close()
			oldSet, err := secrets.PinBindings(context.Background(), txnTestAppID, oldBindings)
			if err != nil {
				t.Fatal(err)
			}
			defer oldSet.Close()
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			state := &DeployState{AppName: txn.AppName, AppID: txnTestAppID, Mode: ModeWorker, Replicas: map[string]*Instance{}}
			worker := &Worker{AppName: txn.AppName, AppID: txnTestAppID, Secrets: txn, Grace: time.Second,
				previousSecrets: map[string]*secrets.PinnedSet{}, previousInstances: map[string]*Instance{}}
			var processes []Process
			t.Cleanup(func() {
				for _, proc := range processes {
					_ = proc.Kill()
					_ = proc.Wait()
				}
			})
			for _, key := range []string{"0", "1"} {
				proc, id, err := launchWorkerInstance(bin, append(txn.Set.Overlay(), "PHLIX_WORKER_HELPER=1"), resources.Config{}, key)
				if err != nil {
					t.Fatal(err)
				}
				processes = append(processes, proc)
				state.Replicas[key] = &Instance{Slot: key, PID: id.PID, StartTicks: id.StartTime, BinaryPath: bin, Version: 2, Status: "running", SecretEnvs: []string{"DB"}}
				worker.previousInstances[key] = &Instance{Slot: key, BinaryPath: bin, Version: 1, SecretEnvs: []string{"DB"}}
				worker.previousSecrets[key] = oldSet
			}
			txn.Stamp(state)
			if err := Store(state); err != nil {
				t.Fatal(err)
			}
			previousFactory := workerLauncherFactory
			t.Cleanup(func() { workerLauncherFactory = previousFactory })
			workerLauncherFactory = func(string) WorkerLauncher {
				return func(binary string, env []string, slot string) (Process, processex.ProcessIdentity, error) {
					if mustLoad(t, state.AppName).SecretTxnID != txn.ID {
						t.Fatal("compensation cleared acceptance before restoring whole fleet")
					}
					if fail && slot == "1" {
						return nil, processex.ProcessIdentity{}, errors.New("injected compensation failure")
					}
					proc, id, err := launchWorkerInstance(binary, append(env, "PHLIX_WORKER_HELPER=1"), resources.Config{}, slot)
					if err == nil {
						processes = append(processes, proc)
					}
					return proc, id, err
				}
			}
			err = worker.revertSecretRollout(context.Background(), state, &WorkerRollout{Status: WorkerRolloutReverting})
			if (err != nil) != fail {
				t.Fatal("unexpected compensation outcome", err)
			}
			outcome, err := ReconcileSecretTxn(state.AppName, txnTestAppID)
			if err != nil {
				t.Fatal(err)
			}
			want := "DB=previous-compensation-value"
			if fail {
				want = "DB=candidate-compensation-value"
				if outcome != SecretTxnCommitted {
					t.Fatal("interrupted compensation lost accepted candidates")
				}
			} else if outcome != SecretTxnDiscarded {
				t.Fatal("completed compensation committed failed deployment")
			}
			for _, inst := range mustLoad(t, state.AppName).Replicas {
				actual, err := RuntimeSecrets(inst, oldBindings)
				if err != nil {
					t.Fatal(err)
				}
				if actual.Overlay()[0] != want {
					t.Fatal("recovered fleet has wrong credentials")
				}
				actual.Close()
			}
		})
	}
}
