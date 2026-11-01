package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/project"
)

func TestMain(m *testing.M) {
	if os.Getenv("PHLIX_WORKER_HELPER") == "1" {
		if os.Getenv("PHLIX_WORKER_HELPER_MODE") == "exit0" {
			os.Exit(0)
		}
		time.Sleep(60 * time.Second)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestEvaluatePolicyMatrix(t *testing.T) {
	cases := []struct {
		policy   string
		exitCode int
		want     Decision
	}{
		{project.RestartAlways, 0, DecisionRestart},
		{project.RestartAlways, 1, DecisionRestart},
		{project.RestartOnFailure, 0, DecisionComplete},
		{project.RestartOnFailure, 1, DecisionRestart},
		{project.RestartOnFailure, -1, DecisionRestart},
		{project.RestartNever, 0, DecisionComplete},
		{project.RestartNever, 1, DecisionFail},
	}
	for _, tc := range cases {
		if got := Evaluate(tc.policy, tc.exitCode); got != tc.want {
			t.Errorf("Evaluate(%q, %d) = %v, want %v", tc.policy, tc.exitCode, got, tc.want)
		}
	}
}

func TestBackoffLadder(t *testing.T) {
	initial, max := 2*time.Second, 60*time.Second
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 2 * time.Second},
		{1, 4 * time.Second},
		{2, 8 * time.Second},
		{3, 16 * time.Second},
		{4, 32 * time.Second},
		{5, 60 * time.Second},
		{50, 60 * time.Second},
	}
	for _, tc := range cases {
		if got := Backoff(initial, max, 2, tc.attempt); got != tc.want {
			t.Errorf("Backoff(attempt=%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestApplyJitter(t *testing.T) {
	base := time.Second
	if got := ApplyJitter(base, 0); got != base {
		t.Errorf("zero jitter changed the delay: %v", got)
	}
	if got := ApplyJitter(base, 0.2); got != 1200*time.Millisecond {
		t.Errorf("+20%% jitter = %v", got)
	}
	if got := ApplyJitter(base, -0.2); got != 800*time.Millisecond {
		t.Errorf("-20%% jitter = %v", got)
	}
	if got := ApplyJitter(base, 5); got != 1200*time.Millisecond {
		t.Errorf("out-of-range jitter must clamp: %v", got)
	}
}

// harness drives one supervisor against a temp project with real helper
// processes, so identity checks and liveness run for real.
type harness struct {
	t       *testing.T
	dir     string
	app     string
	s       *Supervisor
	bin     string
	mode    string
	mu      sync.Mutex
	procs   []*exec.Cmd
	spawns  int
	exitErr error
}

func newHarness(t *testing.T, config string) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, project.FileName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, app: "worker-app", s: New(), bin: bin, mode: "run"}
	h.s.launch = func(appName string, state *deploy.DeployState, slot string) (deploy.Process, error) {
		h.mu.Lock()
		mode := h.mode
		h.spawns++
		h.mu.Unlock()
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "PHLIX_WORKER_HELPER=1", "PHLIX_WORKER_HELPER_MODE="+mode)
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.procs = append(h.procs, cmd)
		h.mu.Unlock()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		})
		id, err := processex.Identity(cmd.Process.Pid)
		if err != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return nil, err
		}
		if inst := state.Replicas[slot]; inst != nil {
			inst.BinaryPath = bin
		}
		deploy.MarkWorkerLaunched(state, slot, id)
		return &testProc{cmd: cmd}, nil
	}
	return h
}

// testProc adapts exec.Cmd to deploy.Process for the supervisor's handle.
type testProc struct {
	cmd *exec.Cmd
}

func (p *testProc) PID() int                   { return p.cmd.Process.Pid }
func (p *testProc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
func (p *testProc) Kill() error                { return p.cmd.Process.Kill() }
func (p *testProc) Wait() error                { return p.cmd.Wait() }

func (h *harness) writeConfig(config string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, project.FileName), []byte(config), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeState(status string, restartCount int) {
	h.t.Helper()
	state := &deploy.DeployState{
		AppName: h.app,
		Mode:    deploy.ModeWorker,
		Replicas: map[string]*deploy.Instance{
			"0": {Slot: "0", PID: 2147483000, BinaryPath: "/nonexistent-worker-binary", Status: status, RestartCount: restartCount},
		},
	}
	if err := deploy.Store(state); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) state() *deploy.DeployState {
	h.t.Helper()
	state, err := deploy.Load(h.app)
	if err != nil {
		h.t.Fatalf("deploy.Load: %v", err)
	}
	return state
}

func (h *harness) replica() *deploy.Instance {
	h.t.Helper()
	return h.state().Replicas["0"]
}

func (h *harness) tick() { h.s.reconcileApp(h.app, h.dir) }

const restartCfg = `
name: worker-app
type: worker
process:
  restart:
    policy: on-failure
    max_attempts: 2
    backoff:
      initial: 10ms
      max: 20ms
      multiplier: 2
  shutdown:
    grace_period: 10ms
`

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func TestCrashLoopWithBackoff(t *testing.T) {
	h := newHarness(t, restartCfg)
	h.writeState("running", 0)

	h.tick()
	inst := h.replica()
	if inst.Status != statusRestarting || inst.RestartCount != 1 {
		t.Fatalf("after first crash: status=%s restarts=%d", inst.Status, inst.RestartCount)
	}
	if inst.LastExitCode != -1 {
		t.Errorf("adopted death must record unknown exit, got %d", inst.LastExitCode)
	}

	waitFor(t, 5*time.Second, func() bool {
		h.tick()
		return h.spawnCount() == 1
	}, "relaunch after backoff never happened")
	if h.replica().Status != statusStarting {
		t.Fatalf("fresh replica must be starting, got %s", h.replica().Status)
	}

	h.mu.Lock()
	proc := h.procs[0]
	h.mu.Unlock()
	_ = proc.Process.Kill()
	_, _ = proc.Process.Wait()

	waitFor(t, 5*time.Second, func() bool {
		h.tick()
		return h.replica().Status == statusRestarting && h.replica().RestartCount == 2
	}, "second crash was not scheduled")

	waitFor(t, 5*time.Second, func() bool {
		h.tick()
		return h.spawnCount() == 2 && h.replica().Status == statusStarting
	}, "second relaunch missing")
	h.mu.Lock()
	proc = h.procs[1]
	h.mu.Unlock()
	_ = proc.Process.Kill()
	_, _ = proc.Process.Wait()

	waitFor(t, 5*time.Second, func() bool {
		h.tick()
		return h.replica().Status == "crashloop"
	}, "third crash with max_attempts=2 must enter crashloop")

	h.tick()
	h.tick()
	if h.spawnCount() != 2 {
		t.Errorf("crashloop must not spawn further replicas: spawns=%d", h.spawnCount())
	}
}

func TestExecHealthGatesStartupAndRestarts(t *testing.T) {
	h := newHarness(t, `
name: worker-app
type: worker
process:
  restart:
    policy: on-failure
    max_attempts: 5
    backoff:
      initial: 5ms
      max: 10ms
  shutdown:
    grace_period: 10ms
health:
  mode: exec
  exec:
    command: ["/bin/true"]
    interval: 10ms
    timeout: 1s
    retries: 2
`)
	h.writeState(statusStarting, 0)

	h.tick()
	waitFor(t, 5*time.Second, func() bool {
		h.tick()
		return h.replica().Status == statusRunning
	}, "healthy replica never became running")
	if h.replica().ReadyAt.IsZero() {
		t.Error("ReadyAt must be stamped when the startup gate passes")
	}

	h.writeConfig(`
name: worker-app
type: worker
process:
  restart:
    policy: on-failure
    max_attempts: 5
    backoff:
      initial: 5ms
      max: 10ms
  shutdown:
    grace_period: 10ms
health:
  mode: exec
  exec:
    command: ["/bin/false"]
    interval: 10ms
    timeout: 1s
    retries: 2
`)
	waitFor(t, 15*time.Second, func() bool {
		h.tick()
		st := h.replica().Status
		return st == statusRestarting || st == statusStarting
	}, "unhealthy replica was never restarted")
	if reason := h.replica().LastExitReason; !strings.Contains(reason, "unhealthy") {
		t.Errorf("exit reason must name the health failure: %q", reason)
	}
}

func TestCleanExitUnderOnFailureCompletes(t *testing.T) {
	h := newHarness(t, `
name: worker-app
type: worker
process:
  restart:
    policy: on-failure
    max_attempts: 5
`)
	h.writeState("running", 0)
	h.s.handles[replicaKey{app: h.app, slot: "0"}] = &handleEntry{
		proc: nil,
		done: make(chan error, 1),
	}
	h.s.handles[replicaKey{app: h.app, slot: "0"}].done <- nil

	h.tick()
	inst := h.replica()
	if inst.Status != "completed" {
		t.Fatalf("clean exit under on-failure must be completed, got %s (reason %q)", inst.Status, inst.LastExitReason)
	}
	if inst.LastExitCode != 0 {
		t.Errorf("clean exit code must be 0, got %d", inst.LastExitCode)
	}
	if h.spawnCount() != 0 {
		t.Errorf("completed replica must not restart, spawns=%d", h.spawnCount())
	}
}

func TestNeverPolicyFailsOnAbnormalExit(t *testing.T) {
	h := newHarness(t, `
name: worker-app
type: worker
process:
  restart:
    policy: never
`)
	h.writeState("running", 0)
	h.tick()
	if inst := h.replica(); inst.Status != "failed" {
		t.Fatalf("abnormal exit under never must be failed, got %s", inst.Status)
	}
}

func TestStableWindowResetsCounter(t *testing.T) {
	h := newHarness(t, `
name: worker-app
type: worker
process:
  restart:
    policy: on-failure
    max_attempts: 1
`)
	h.writeState("running", 0)
	h.tick()
	waitFor(t, 5*time.Second, func() bool {
		h.tick()
		return h.spawnCount() == 1 && h.replica().Status == statusRunning
	}, "replica was not relaunched and readied")

	h.mu.Lock()
	cmd := h.procs[0]
	h.mu.Unlock()
	state := h.state()
	inst := state.Replicas["0"]
	old := time.Now().Add(-2 * project.StableWindow)
	inst.StartedAt = old
	inst.ReadyAt = old
	inst.RestartCount = 1
	if err := deploy.Store(state); err != nil {
		t.Fatal(err)
	}
	_ = cmd

	h.tick()
	inst = h.replica()
	if inst.RestartCount != 0 {
		t.Errorf("stable window must reset the counter, got %d", inst.RestartCount)
	}
	if inst.Status != statusRunning {
		t.Errorf("status = %s", inst.Status)
	}
}

func (h *harness) spawnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spawns
}
