package deploy

import (
	"context"
	"os"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/resources"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

func TestSecretWorkerRestartPreservesFleetOnFailure(t *testing.T) {
	for _, fail := range []bool{true, false} {
		name := "successful"
		if fail {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			txnTestEnv(t)
			seedSecret(t, "db", "previous-runtime-credential")
			bindings := []secrets.Binding{{Secret: "db", Env: "DB"}}
			if err := secrets.ReplaceBindings(txnTestAppID, bindings); err != nil {
				t.Fatal(err)
			}
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			state := &DeployState{AppName: "restart-" + name, AppID: txnTestAppID, Mode: ModeWorker, Replicas: map[string]*Instance{}}
			var processes []Process
			t.Cleanup(func() {
				for _, proc := range processes {
					_ = proc.Kill()
					_ = proc.Wait()
				}
			})
			for _, key := range []string{"0", "1"} {
				proc, id, err := launchWorkerInstance(bin, []string{"PHLIX_WORKER_HELPER=1", "DB=previous-runtime-credential"}, resources.Config{}, key)
				if err != nil {
					t.Fatal(err)
				}
				processes = append(processes, proc)
				state.Replicas[key] = &Instance{Slot: key, PID: id.PID, StartTicks: id.StartTime, BinaryPath: bin, Status: "running", SecretEnvs: []string{"DB"}}
			}
			if err := Store(state); err != nil {
				t.Fatal(err)
			}
			seedSecret(t, "db", "desired-runtime-credential")
			desired, err := secrets.PinBindings(context.Background(), txnTestAppID, bindings)
			if err != nil {
				t.Fatal(err)
			}
			defer desired.Close()
			previousFactory := workerLauncherFactory
			defer func() { workerLauncherFactory = previousFactory }()
			workerLauncherFactory = func(string) WorkerLauncher {
				return func(binary string, env []string, slot string) (Process, processex.ProcessIdentity, error) {
					store, err := secrets.Open(txnTestAppID)
					if err != nil || store.Pending == nil {
						t.Fatal("restart touched runtime before writing intent")
					}
					if fail && slot == "1" {
						return nil, processex.ProcessIdentity{}, phelixerr.New(phelixerr.CodeInstanceStartFailed, "injected launch failure")
					}
					proc, id, err := launchWorkerInstance(binary, append(env, "PHLIX_WORKER_HELPER=1"), resources.Config{}, slot)
					if err == nil {
						processes = append(processes, proc)
					}
					return proc, id, err
				}
			}
			err = RestartSecretInstances(context.Background(), state, desired, &Worker{Grace: time.Second}, "")
			if (err != nil) != fail {
				t.Fatalf("restart failure=%v: %v", fail, err)
			}
			current := mustLoad(t, state.AppName)
			for _, inst := range current.Replicas {
				if !WorkerInstanceAlive(inst) {
					t.Fatal("restart left a dead active replica")
				}
				runtime, err := RuntimeSecrets(inst, bindings)
				if err != nil {
					t.Fatal(err)
				}
				want := "DB=desired-runtime-credential"
				if fail {
					want = "DB=previous-runtime-credential"
				}
				if runtime.Overlay()[0] != want {
					t.Fatal("runtime credentials do not match restart outcome")
				}
				runtime.Close()
			}
			store, err := secrets.Open(txnTestAppID)
			if err != nil || store.Pending != nil {
				t.Fatal("restart intent did not settle", err)
			}
			resolved, err := secrets.Resolve(context.Background(), txnTestAppID, "db")
			if err != nil || resolved.Value != "desired-runtime-credential" {
				t.Fatal("failed restart lost desired value", err)
			}
		})
	}
}
