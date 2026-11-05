package app

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/secrets"
	"github.com/shirou/gopsutil/process"
)

func TestLoadStatePreservesLiveOutputOwnership(t *testing.T) {
	isolateStateFile(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	file, err := os.CreateTemp(t.TempDir(), "app-log")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	resource := &fakeResourceTracker{}
	value := "owned-classic-runtime-value"
	release := phelixerr.AcquireSecretValues([]string{value})
	defer release()
	m := &AppManager{Apps: map[string]*AppInfo{"owned": {
		ID: "owned", Name: "owned", Status: "running", PID: cmd.Process.Pid, Cmd: cmd,
		logFileHandle: file, resourceInstance: resource, releaseSecrets: release,
	}}}
	if err := m.SaveState(); err != nil {
		t.Fatal(err)
	}
	if err := m.LoadState(); err != nil {
		t.Fatal(err)
	}
	current := m.Apps["owned"]
	if current.Cmd != cmd || current.resourceInstance != resource || current.logFileHandle != file || current.releaseSecrets == nil {
		t.Fatal("reload dropped ownership of a live process")
	}
	if err := m.StopApplication("owned"); err != nil {
		t.Fatal(err)
	}
	if !resource.closed || phelixerr.Redact(value) != value {
		t.Fatal("stopped process retained resources or credential references")
	}
}

func TestClassicAcceptanceFailureRestoresPreviousCredentials(t *testing.T) {
	isolateStateFile(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	const id = "0f0e1d2c-3b4a-4958-8677-8899aabbccdd"
	m := &AppManager{Apps: map[string]*AppInfo{}}
	writeFixtureApp(t, m, id, true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if _, err := secrets.Set(id, "db", "old-classic-value"); err != nil {
		t.Fatal(err)
	}
	bindings := []secrets.Binding{{Secret: "db", Env: "DB"}}
	if err := secrets.ReplaceBindings(id, bindings); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveState(); err != nil {
		t.Fatal(err)
	}
	if err := m.StartApplication(id, port, id); err != nil {
		t.Fatal(err)
	}
	defer m.StopApplication(id)
	if _, err := secrets.Set(id, "db", "new-classic-value"); err != nil {
		t.Fatal(err)
	}
	pinned, err := secrets.PinBindings(context.Background(), id, bindings)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	failure := errors.New("acceptance write failed")
	err = m.RestartApplicationWithSecrets(id, pinned, func() error { return failure })
	if !errors.Is(err, failure) {
		t.Fatal("restart hid journal failure", err)
	}
	runtime, err := secrets.RuntimeForPID(m.Apps[id].PID, []string{"DB"})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Overlay()[0] != "DB=old-classic-value" {
		t.Fatal("failed acceptance changed runtime credentials")
	}
}

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
