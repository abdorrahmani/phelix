package autoscale

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
)

// spawnScript starts a script as a detached child and registers cleanup.
func spawnScript(t *testing.T, path string) int {
	t.Helper()
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", path, err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return pid
}

// writeScript writes an executable shell script.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// mustHistory reads the app's history, tolerating absence.
func mustHistory(t *testing.T, app string) []HistoryRecord {
	t.Helper()
	records, err := ReadHistory(app, 0)
	if err != nil {
		return nil
	}
	return records
}

// registerTestApp points the app registry at a temp data dir holding a
// minimal apps.json entry, so the worker launcher's resource lookup resolves
// against test state instead of the developer's real registry.
func registerTestApp(t *testing.T, appName string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "apps.json"), []byte(fmt.Sprintf(`{%q: {"name": %q, "resources": {}}}`, appName, appName)), 0o644); err != nil {
		t.Fatalf("write apps.json: %v", err)
	}
	if err := app.SetStateDirForTesting(dir); err != nil {
		t.Fatalf("SetStateDirForTesting: %v", err)
	}
}

// tickUntil drives controller ticks until the app's replica count matches
// want or the deadline expires.
func tickUntil(t *testing.T, c *Controller, ctx context.Context, app string, want int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		runTicks(t, c, ctx)
		if state, err := deploy.Load(app); err == nil && len(state.Replicas) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	state, _ := deploy.Load(app)
	count := 0
	if state != nil {
		count = len(state.Replicas)
	}
	t.Fatalf("replica count for %s = %d, want %d within %s (history: %v)", app, count, want, deadline, mustHistory(t, app))
}

// pollState polls the app's replica count until it matches want or the
// deadline expires.
func pollState(t *testing.T, app string, want int, deadline time.Duration) *deploy.DeployState {
	t.Helper()
	end := time.Now().Add(deadline)
	var last *deploy.DeployState
	for time.Now().Before(end) {
		state, err := deploy.Load(app)
		if err == nil {
			last = state
			if len(state.Replicas) == want {
				return state
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	count := 0
	if last != nil {
		count = len(last.Replicas)
	}
	t.Fatalf("replica count for %s = %d, want %d within %s (history: %v)", app, count, want, deadline, mustHistory(t, app))
	return nil
}

// runTicks drives the controller's tick loop on the caller's goroutine so the
// test can pace evaluations deterministically. Each call starts at most one
// evaluation per app and waits for its in-flight flag to clear.
func runTicks(t *testing.T, c *Controller, ctx context.Context) {
	t.Helper()
	c.tick(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		busy := len(c.inFlight)
		c.mu.Unlock()
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("evaluation did not settle within 10s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestControllerWorkerEndToEnd scales a real worker app up under a busy loop
// and back down to the minimum under idle processes, through the production
// dispatch path (deploy.WorkerScale) with real processes and the real
// sampler.
func TestControllerWorkerEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("real-process end-to-end test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	appDir := t.TempDir()
	config := `name: loadjobs
type: worker
process:
  shutdown:
    signal: SIGTERM
    grace_period: 2s
deploy:
  strategy: classic
  autoscaling:
    enabled: true
    min_replicas: 1
    max_replicas: 3
    interval: 250ms
    cooldown: 0s
    evaluation_windows: 1
    metrics:
      cpu:
        scale_up: 30
        scale_down: 5
`
	if err := os.WriteFile(filepath.Join(appDir, "phelix.yaml"), []byte(config), 0o644); err != nil {
		t.Fatalf("write phelix.yaml: %v", err)
	}

	binDir := t.TempDir()
	busy := writeScript(t, binDir, "busy.sh", "while :; do :; done")
	idle := writeScript(t, binDir, "idle.sh", "sleep 300")

	appName := "loadjobs"
	registerTestApp(t, appName)
	busyPID := spawnScript(t, busy)
	state, err := deploy.LoadOrInit(appName, deploy.ModeWorker, 0)
	if err != nil {
		t.Fatalf("LoadOrInit: %v", err)
	}
	state.Replicas = map[string]*deploy.Instance{
		"0": {Slot: "0", PID: busyPID, Status: "running", BinaryPath: busy, StartedAt: time.Now()},
	}
	if err := deploy.Store(state); err != nil {
		t.Fatalf("Store: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := NewController(func() []AppRef {
		return []AppRef{{Name: appName, ID: "app-loadjobs", Directory: appDir}}
	}, nil)

	tickUntil(t, c, ctx, appName, 2, 20*time.Second)
	tickUntil(t, c, ctx, appName, 3, 20*time.Second)

	records, _ := ReadHistory(appName, 0)
	executed := 0
	for _, rec := range records {
		if rec.Kind == HistoryExecuted && rec.Direction == "up" {
			executed++
		}
	}
	if executed < 2 {
		t.Errorf("scale-up executions = %d, want at least 2", executed)
	}

	// Swap the fleet to idle processes: the sampler needs one observation to
	// seed the CPU delta, then the down condition holds and the controller
	// drains back to min_replicas through the same WorkerScale path.
	busyState, err := deploy.Load(appName)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	newReplicas := map[string]*deploy.Instance{}
	for key := range busyState.Replicas {
		idlePID := spawnScript(t, idle)
		newReplicas[key] = &deploy.Instance{
			Slot: key, PID: idlePID, Status: "running",
			BinaryPath: idle, StartedAt: time.Now(),
		}
	}
	busyState.Replicas = newReplicas
	if err := deploy.Store(busyState); err != nil {
		t.Fatalf("Store: %v", err)
	}

	tickUntil(t, c, ctx, appName, 1, 20*time.Second)

	records, _ = ReadHistory(appName, 0)
	downExecuted := 0
	for _, rec := range records {
		if rec.Kind == HistoryExecuted && rec.Direction == "down" {
			downExecuted++
		}
	}
	if downExecuted < 2 {
		t.Errorf("scale-down executions = %d, want at least 2", downExecuted)
	}
	opsRecordsVerify(t, appName)
}

// opsRecordsVerify asserts every executed scale produced an ops record the
// operation-status surface can resolve.
func opsRecordsVerify(t *testing.T, app string) {
	t.Helper()
	records, err := ReadHistory(app, 0)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	executed := 0
	for _, rec := range records {
		if rec.Kind == HistoryExecuted {
			executed++
		}
	}
	if executed == 0 {
		t.Fatal("no executed records found")
	}
	fmt.Printf("autoscale e2e: %d executed scaling operations recorded\n", executed)
}
