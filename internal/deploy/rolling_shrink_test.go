package deploy

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Helper child used to prove surplus replicas are REALLY terminated: it parks
// reading stdin and exits when the parent disappears or signals it.
func TestHelperParkedProcess(t *testing.T) {
	if os.Getenv("PHELIX_PARKED_HELPER") != "1" {
		return
	}
	buf := make([]byte, 1)
	for {
		if _, err := os.Stdin.Read(buf); err != nil {
			os.Exit(0)
		}
	}
}

// Bug regression: shrinking --replicas used to drop state entries while the
// surplus processes kept running forever. The rollout must terminate them.
func TestRolling_ShrinkDrainsSurplusInstance(t *testing.T) {
	resetHome(t)
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve executable: %v", err)
	}

	cmd := exec.Command(exe, "-test.run=^TestHelperParkedProcess$")
	cmd.Env = append(os.Environ(), "PHELIX_PARKED_HELPER=1")
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = stdinR
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn parked helper: %v", err)
	}
	childPID := cmd.Process.Pid
	t.Cleanup(func() {
		_ = stdinW.Close() // EOF ends the helper even without a signal
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	state := &DeployState{
		AppName: "roll-shrink", AppID: "rs", Mode: ModeRolling, PublicPort: 8114,
		Replicas: map[string]*Instance{
			// Old instances of kept replicas: PID is the test binary itself but
			// BinaryPath deliberately mismatches, exercising the identity gate's
			// refuse-to-signal path (an unrelated process must never be hit).
			"0": {Slot: "0", PID: os.Getpid(), Port: 1, BinaryPath: "/bin/true", Status: "running"},
			// Surplus replica: real parked child with correct identity so the
			// drain can actually signal it.
			"1": {Slot: "1", PID: childPID, Port: 2, BinaryPath: exe, Status: "running"},
			"2": {Slot: "2", PID: os.Getpid(), Port: 3, BinaryPath: "/bin/true", Status: "running"},
		},
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}

	fl := &multiLauncher{}
	defer fl.close()
	ac := &auditedClient{inner: &fakeProxyClient{alive: true},
		statusRows: []proxy.AppStatus{{AppName: "roll-shrink"}}}

	r := &Rolling{
		AppName: "roll-shrink", AppID: "rs", PublicPort: 8114, Replicas: 2,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    ac,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastCfg() },
		GracePeriod:    time.Second,
	}
	if err := r.Deploy(context.Background()); err != nil {
		t.Fatalf("shrinking rollout: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(childPID) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(childPID) {
		t.Fatalf("surplus replica pid %d survived the shrink", childPID)
	}

	// The parent test process must have been left strictly alone (identity
	// gate): if it had been signalled we would already be dying.
	st := mustLoad(t, "roll-shrink")
	if len(st.Replicas) != 2 {
		t.Fatalf("state has %d replicas, want 2", len(st.Replicas))
	}
	for k, inst := range st.Replicas {
		if inst.Status != "running" || inst.Port == 0 || inst.Port == 2 {
			t.Fatalf("replica %s wrong end state: %+v", k, inst)
		}
	}
}
