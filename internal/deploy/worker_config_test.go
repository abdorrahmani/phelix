package deploy

import (
	"os"
	"testing"

	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/project"
)

// The snapshot must carry the resolved supervision policy, not the raw file
// values, so the dashboard shows what replicas actually run under.
func TestWorkerConfigState_ResolvesPolicyAndHealth(t *testing.T) {
	cfg := &project.Config{
		Type: project.TypeWorker,
		Process: &project.ProcessConfig{
			Restart: &project.RestartConfig{
				Policy:      project.RestartAlways,
				MaxAttempts: 7,
				Backoff: &project.BackoffConfig{
					Initial:    "2s",
					Max:        "45s",
					Multiplier: 3,
				},
			},
			Shutdown: &project.ShutdownConfig{
				Signal:      project.ShutdownSIGINT,
				GracePeriod: "12s",
			},
			StartupTimeout: "20s",
		},
		Health: &project.HealthConfig{
			Mode: project.WorkerHealthExec,
			Exec: &project.ExecHealthConfig{
				Command:  []string{"./healthcheck"},
				Interval: "3s",
				Timeout:  "4s",
				Retries:  2,
			},
		},
		Deploy: &project.DeployConfig{
			Rolling: &project.RollingDeployConfig{MaxSurge: 1, MaxUnavailable: 0, DrainTimeout: "9s"},
		},
	}

	got := workerConfigState(cfg)
	if got == nil {
		t.Fatal("expected a worker config")
	}
	if got.RestartPolicy != project.RestartAlways || got.MaxRestartAttempts != 7 {
		t.Fatalf("restart = %+v", got)
	}
	if got.BackoffInitial != "2s" || got.BackoffMax != "45s" || got.BackoffMultiplier != 3 {
		t.Fatalf("backoff = %+v", got)
	}
	if got.ShutdownSignal != project.ShutdownSIGINT || got.GracePeriod != "12s" || got.StartupTimeout != "20s" {
		t.Fatalf("shutdown = %+v", got)
	}
	if got.HealthMode != project.WorkerHealthExec || len(got.HealthCommand) != 1 ||
		got.HealthCommand[0] != "./healthcheck" || got.HealthInterval != "3s" ||
		got.HealthTimeout != "4s" || got.HealthRetries != 2 {
		t.Fatalf("health = %+v", got)
	}
	if got.RollingMaxSurge != 1 || got.RollingMaxUnavail != 0 || got.RollingDrain != "9s" {
		t.Fatalf("rolling = %+v", got)
	}
}

func TestWorkerConfigState_NonWorkerIsNil(t *testing.T) {
	if got := workerConfigState(&project.Config{Type: project.TypeService}); got != nil {
		t.Fatalf("non-worker must carry no worker block, got %+v", got)
	}
}

func TestWorkerAlivePIDs_SkipsDeadReplicasInOrder(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	id, err := processex.Identity(os.Getpid())
	if err != nil {
		t.Fatalf("process identity: %v", err)
	}
	state := &DeployState{
		Replicas: map[string]*Instance{
			"0": {Slot: "0", PID: os.Getpid(), StartTicks: id.StartTime, BinaryPath: exe},
			"1": {Slot: "1", PID: 0},
			"2": {Slot: "2", PID: 999999, BinaryPath: exe},
		},
	}
	pids := WorkerAlivePIDs(state)
	if len(pids) != 1 || pids[0] != os.Getpid() {
		t.Fatalf("alive PIDs = %v, want [%d]", pids, os.Getpid())
	}
}
