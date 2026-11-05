package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/resources"
)

func TestMain(m *testing.M) {
	if os.Getenv("PHLIX_WORKER_HELPER") == "1" {
		if os.Getenv("PORT") != "" {
			os.Exit(7)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestLaunchWorkerInstance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PORT", "12345")
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	proc, id, err := launchWorkerInstance(bin, []string{"PHLIX_WORKER_HELPER=1"}, resources.Config{}, "0")
	if err != nil {
		t.Fatalf("launchWorkerInstance: %v", err)
	}
	defer func() { _ = proc.Kill(); _ = proc.Wait() }()

	if proc.PID() != id.PID {
		t.Errorf("identity pid %d != process pid %d", id.PID, proc.PID())
	}
	if id.StartTime == 0 {
		t.Error("start time must be captured at spawn")
	}
	if !processex.Matches(id) {
		t.Error("freshly spawned replica must match its identity")
	}

	time.Sleep(150 * time.Millisecond)
	if !processex.Matches(id) {
		t.Fatal("replica died prematurely; a PORT injection would exit it with code 7")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	logPath := filepath.Join(home, ".phelix", "logs", "deploy_"+filepath.Base(bin)+"_replica-0.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("per-replica log missing: %v", err)
	}
}

func TestWorkerLogTag(t *testing.T) {
	if got := workerLogTag("3"); got != "replica-3" {
		t.Errorf("workerLogTag = %q", got)
	}
}

func TestWorkerStopRetainsUnconfirmedInstance(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	proc, id, err := launchWorkerInstance(bin, []string{"PHLIX_WORKER_HELPER=1"}, resources.Config{}, "0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Kill(); _ = proc.Wait() }()
	state := &DeployState{AppName: "stop-uncertain", Mode: ModeWorker, Replicas: map[string]*Instance{
		"0": {PID: id.PID, StartTicks: id.StartTime, BinaryPath: id.ExePath, Status: "running"},
	}}
	refused := newFakeProc(0)
	refused.killErr = errors.New("kill denied")
	defer close(refused.exitCh)
	report, err := StopWorkerReplica(state, "0", nil, time.Millisecond, refused)
	if err == nil || report.Exited || state.Replicas["0"].PID != id.PID || state.Replicas["0"].Status == "stopped" {
		t.Fatal("unconfirmed stop lost process record", err)
	}
	stored, err := Load(state.AppName)
	if err != nil || stored.Replicas["0"].PID != id.PID {
		t.Fatal("unconfirmed stop lost durable record", err)
	}
}

func TestGracefulStopWaitsOnce(t *testing.T) {
	proc := newFakeProc(time.Millisecond)
	report, err := GracefulStopSignal(nil, context.Background(), proc, time.Second, 0)
	if err != nil || !report.Exited || report.waitErr != nil {
		t.Fatal("stop lost wait result", err)
	}
}
