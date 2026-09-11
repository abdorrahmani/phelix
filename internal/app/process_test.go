package app

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/shirou/gopsutil/process"
)

func TestIsProcessRunningTreatsZombieAsStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("zombie processes are a Unix concept")
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill child process: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p, err := process.NewProcess(int32(cmd.Process.Pid))
		if err == nil {
			status, statusErr := p.Status()
			if statusErr == nil && status == "Z" {
				manager := &AppManager{}
				if manager.isProcessRunning(cmd.Process.Pid) {
					t.Fatal("zombie process was considered running")
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("child process did not become a zombie")
}
