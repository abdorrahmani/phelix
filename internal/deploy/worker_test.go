package deploy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/processex"
)

// workerEngineHelper re-executes the test binary as a helper worker; the
// helper branch itself lives in the package TestMain.
func workerEngineHelper() {
	if os.Getenv("PHLIX_WORKER_HELPER") == "1" {
		time.Sleep(60 * time.Second)
		os.Exit(1)
	}
}

// versionSource is a BuildSource serving one recorded version.
type versionSource struct {
	app string
	ver int
}

func (s *versionSource) Build(_ context.Context) (string, string, error) {
	return VersionPaths(s.app, s.ver)
}

func (s *versionSource) Describe() string   { return fmt.Sprintf("v%d", s.ver) }
func (s *versionSource) TargetVersion() int { return s.ver }

// spawnHelper replaces the worker launcher factory: it launches the recorded
// binary (a copy of the test binary) with the helper environment set.
func spawnHelper(t *testing.T) {
	t.Helper()
	prev := workerLauncherFactory
	workerLauncherFactory = func(appName string) WorkerLauncher {
		return func(binaryPath string, env []string, replica string) (Process, processex.ProcessIdentity, error) {
			cmd := exec.Command(binaryPath)
			cmd.Env = append(os.Environ(), "PHLIX_WORKER_HELPER=1", "PHLIX_WORKER_HELPER_MODE=run")
			if err := cmd.Start(); err != nil {
				return nil, processex.ProcessIdentity{}, err
			}
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})
			id, err := processex.Identity(cmd.Process.Pid)
			if err != nil {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
				return nil, processex.ProcessIdentity{}, err
			}
			return &engineProc{cmd: cmd}, id, nil
		}
	}
	t.Cleanup(func() { workerLauncherFactory = prev })
}

// engineProc adapts exec.Cmd to Process.
type engineProc struct{ cmd *exec.Cmd }

func (p *engineProc) PID() int                   { return p.cmd.Process.Pid }
func (p *engineProc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
func (p *engineProc) Kill() error                { return p.cmd.Process.Kill() }
func (p *engineProc) Wait() error                { return p.cmd.Wait() }

func appState(t *testing.T, appName string) *DeployState {
	t.Helper()
	state, err := Load(appName)
	if err != nil {
		t.Fatalf("Load(%s): %v", appName, err)
	}
	return state
}

// recordVersion creates a real recorded version (builds/vN + env/vN.enc) for
// the app, pointing at the test binary.
func recordVersion(t *testing.T, app, id, bin string) int {
	t.Helper()
	rec, err := RecordFreshBuild(app, id, bin, "", "", nil, DefaultRetention{Max: 5}, &nopLogger{})
	if err != nil {
		t.Fatalf("RecordFreshBuild: %v", err)
	}
	return rec.Version
}

func TestWorkerDeployRollingReplacement(t *testing.T) {
	workerEngineHelper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	app, id := "workerapp", "42"
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spawnHelper(t)

	v1 := recordVersion(t, app, id, bin)
	eng := &Worker{
		AppName:  app,
		AppID:    id,
		Source:   &versionSource{app: app, ver: v1},
		Strategy: workerStrategyRolling,
		Replicas: 2,
		Dir:      home,
		Signal:   mustSignal(t, "SIGTERM"),
		Grace:    2 * time.Second,
	}
	if err := eng.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	state := appState(t, app)
	if state.Mode != ModeWorker || len(state.Replicas) != 2 {
		t.Fatalf("state after first deploy: mode=%s replicas=%d", state.Mode, len(state.Replicas))
	}
	var firstPIDs []int
	for key, inst := range state.Replicas {
		if inst.Status != "running" || inst.Version != v1 || inst.PID == 0 {
			t.Fatalf("replica %s = %+v", key, inst)
		}
		if !WorkerInstanceAlive(inst) {
			t.Fatalf("replica %s not alive", key)
		}
		if inst.StartTicks == 0 {
			t.Fatalf("replica %s has no start ticks", key)
		}
		firstPIDs = append(firstPIDs, inst.PID)
	}

	v2 := recordVersion(t, app, id, bin)
	eng = &Worker{
		AppName:  app,
		AppID:    id,
		Source:   &versionSource{app: app, ver: v2},
		Strategy: workerStrategyRolling,
		Replicas: 2,
		Dir:      home,
		Signal:   mustSignal(t, "SIGTERM"),
		Grace:    2 * time.Second,
	}
	if err := eng.Deploy(context.Background()); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	state = appState(t, app)
	if state.ActiveVersion != v2 {
		t.Fatalf("active version = %d, want %d", state.ActiveVersion, v2)
	}
	if state.WorkerRollout == nil || state.WorkerRollout.Status != WorkerRolloutDone {
		t.Fatalf("journal = %+v", state.WorkerRollout)
	}
	for key, inst := range state.Replicas {
		if inst.Version != v2 || inst.Status != "running" {
			t.Fatalf("replica %s after roll = %+v", key, inst)
		}
	}
	for _, old := range firstPIDs {
		for _, inst := range state.Replicas {
			if inst.PID == old {
				t.Errorf("old pid %d still recorded", old)
			}
		}
	}
}

func TestWorkerDeployFailureReverts(t *testing.T) {
	workerEngineHelper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	app, id := "workerrev", "43"
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spawnHelper(t)

	v1 := recordVersion(t, app, id, bin)
	eng := &Worker{
		AppName:  app,
		AppID:    id,
		Source:   &versionSource{app: app, ver: v1},
		Strategy: workerStrategyRolling,
		Replicas: 2,
		Dir:      home,
		Signal:   mustSignal(t, "SIGTERM"),
		Grace:    2 * time.Second,
	}
	if err := eng.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	oldPIDs := map[string]int{}
	for key, inst := range appState(t, app).Replicas {
		oldPIDs[key] = inst.PID
	}

	v2 := recordVersion(t, app, id, bin)
	eng = &Worker{
		AppName:        app,
		AppID:          id,
		Source:         &versionSource{app: app, ver: v2},
		Strategy:       workerStrategyRolling,
		Replicas:       2,
		Dir:            home,
		Signal:         mustSignal(t, "SIGTERM"),
		Grace:          2 * time.Second,
		StartupTimeout: 1500 * time.Millisecond,
		Health:         WorkerGate{Mode: WorkerGateExec, Command: []string{"/bin/false"}, Timeout: time.Second},
	}
	err = eng.Deploy(context.Background())
	if err == nil {
		t.Fatal("deploy with a failing gate must fail")
	}
	state := appState(t, app)
	for key, inst := range state.Replicas {
		if inst.Version != v1 || !WorkerInstanceAlive(inst) || inst.PID != oldPIDs[key] {
			t.Fatalf("replica %s must be reverted to the untouched v1 record: %+v", key, inst)
		}
	}
	if state.WorkerRollout == nil || state.WorkerRollout.Status != WorkerRolloutDone {
		t.Fatalf("journal after failed deploy = %+v", state.WorkerRollout)
	}
}

func TestWorkerDeployClassic(t *testing.T) {
	workerEngineHelper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	app, id := "workerclassic", "44"
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spawnHelper(t)

	v1 := recordVersion(t, app, id, bin)
	eng := &Worker{
		AppName:  app,
		AppID:    id,
		Source:   &versionSource{app: app, ver: v1},
		Strategy: workerStrategyClassic,
		Replicas: 3,
		Dir:      home,
		Signal:   mustSignal(t, "SIGTERM"),
		Grace:    2 * time.Second,
	}
	if err := eng.Deploy(context.Background()); err != nil {
		t.Fatalf("classic deploy: %v", err)
	}
	state := appState(t, app)
	if len(state.Replicas) != 3 {
		t.Fatalf("replicas = %d", len(state.Replicas))
	}
	for key, inst := range state.Replicas {
		if inst.Status != "running" || inst.Version != v1 {
			t.Fatalf("replica %s = %+v", key, inst)
		}
	}

	v2 := recordVersion(t, app, id, bin)
	eng.Source = &versionSource{app: app, ver: v2}
	if err := eng.Deploy(context.Background()); err != nil {
		t.Fatalf("second classic deploy: %v", err)
	}
	state = appState(t, app)
	for key, inst := range state.Replicas {
		if inst.Version != v2 || inst.Status != "running" {
			t.Fatalf("replica %s after classic roll = %+v", key, inst)
		}
	}
}

func TestRecoverWorkerRollout(t *testing.T) {
	workerEngineHelper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	app, id := "workerrecover", "45"
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spawnHelper(t)

	v1 := recordVersion(t, app, id, bin)
	eng := &Worker{
		AppName:  app,
		AppID:    id,
		Source:   &versionSource{app: app, ver: v1},
		Strategy: workerStrategyRolling,
		Replicas: 2,
		Dir:      home,
		Signal:   mustSignal(t, "SIGTERM"),
		Grace:    2 * time.Second,
	}
	if err := eng.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	v2 := recordVersion(t, app, id, bin)
	eng.Source = &versionSource{app: app, ver: v2}
	if err := eng.Deploy(context.Background()); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	state := appState(t, app)
	state.WorkerRollout = &WorkerRollout{
		Version:      v2,
		Strategy:     workerStrategyRolling,
		Status:       WorkerRolloutRunning,
		ReplacedFrom: map[string]int{"0": v1, "1": v1},
		StartedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	_ = Store(state)

	if err := RecoverWorkerRollout(context.Background(), app); err != nil {
		t.Fatalf("RecoverWorkerRollout: %v", err)
	}
	state = appState(t, app)
	if state.WorkerRollout == nil || state.WorkerRollout.Status != WorkerRolloutDone {
		t.Fatalf("journal after recovery = %+v", state.WorkerRollout)
	}
	for key, inst := range state.Replicas {
		if inst.Version != v1 || !WorkerInstanceAlive(inst) {
			t.Fatalf("replica %s after recovery = %+v (want v%d alive)", key, inst, v1)
		}
	}
}

func mustSignal(t *testing.T, name string) os.Signal {
	t.Helper()
	sig, err := SignalFromName(name)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func TestWorkerLogPath(t *testing.T) {
	got := workerLogPath("/home/u", "/builds/v3/app_42", "2")
	want := filepath.Join("/home/u", ".phelix", "logs", "deploy_app_42_replica-2.log")
	if got != want {
		t.Errorf("workerLogPath = %q, want %q", got, want)
	}
}
