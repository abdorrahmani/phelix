package deploy

import (
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
