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
	cleared := false
	err = m.RestartApplicationWithSecrets(id, pinned, func(current *AppInfo, accepted bool) error {
		if accepted {
			return failure
		}
		if current == nil {
			if !m.appProcessRunning(m.Apps[id]) {
				t.Fatal("candidate stopped before journal restoration")
			}
			cleared = true
		}
		return nil
	})
	if !errors.Is(err, failure) {
		t.Fatal("restart hid journal failure", err)
	}
	if !cleared {
		t.Fatal("failed acceptance was not cleared before restoration")
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

func TestLoadStateRetainsOwnershipAndFileOnCorruption(t *testing.T) {
	isolateStateFile(t)
	for _, data := range []string{"broken", "null"} {
		info := &AppInfo{ID: "owned", Name: "owned", PID: os.Getpid(), Status: "running"}
		m := &AppManager{Apps: map[string]*AppInfo{"owned": info}}
		if err := os.WriteFile(stateFile, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := m.LoadState(); err == nil {
			t.Fatal("invalid state accepted")
		}
		current, err := os.ReadFile(stateFile)
		if err != nil || string(current) != data || m.Apps["owned"] != info {
			t.Fatal("corruption erased state or ownership", err)
		}
	}
}

func TestRestoreSkipsOnlyAppWhoseGuardFails(t *testing.T) {
	root := isolateStateFile(t)
	previousGuard, previousSkipper := RestoreAppGuard, DeployedAppSkipper
	defer func() { RestoreAppGuard, DeployedAppSkipper = previousGuard, previousSkipper }()
	DeployedAppSkipper = nil
	manager := &AppManager{Apps: map[string]*AppInfo{
		"blocked": {ID: "blocked", Name: "blocked", AutoStart: true, Status: "stopped", Directory: root},
		"ready":   {ID: "ready", Name: "ready", AutoStart: true, Status: "stopped", Directory: root},
	}}
	if err := manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	checked := map[string]bool{}
	released := false
	RestoreAppGuard = func(id, name string) (func(), error) {
		checked[id] = true
		if id == "blocked" {
			return nil, errors.New("deployment in progress")
		}
		return func() { released = true }, nil
	}
	if _, err := manager.RestoreAutoStartApps(); err != nil {
		t.Fatal(err)
	}
	if !checked["blocked"] || !checked["ready"] || !released {
		t.Fatal("blocked app prevented independent restore or leaked its lock")
	}
}

func TestClassicStateRejectsRecycledPID(t *testing.T) {
	isolateStateFile(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	manager := &AppManager{Apps: map[string]*AppInfo{"stale": {ID: "stale", PID: cmd.Process.Pid, StartTicks: 1, Status: "running"}}}
	if err := manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	if err := manager.LoadState(); err != nil {
		t.Fatal(err)
	}
	if manager.Apps["stale"].PID != 0 || manager.Apps["stale"].Status == "running" {
		t.Fatal("recycled PID was adopted")
	}
	if err := manager.StopApplication("stale"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("unrelated process was stopped")
	}
}
