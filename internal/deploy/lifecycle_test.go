package deploy

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
)

// selfBinary returns the path of the running test binary — a real executable
// whose /proc/<pid>/exe resolves to itself, usable to build an Instance that
// InstanceAlive must accept.
func selfBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// spawnSleeper starts a real `sleep` child and returns it with its recorded
// Instance so tests can exercise identity-verified stopping end to end.
func spawnSleeper(t *testing.T) (*exec.Cmd, *Instance) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	_ = exe
	return cmd, &Instance{Slot: SlotBlue, PID: cmd.Process.Pid, BinaryPath: mustSleepPath(t)}
}

// mustSleepPath resolves the real `sleep` executable path the same way
// processExecutable would (via /proc), so sameExecutable matches.
func mustSleepPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	return real
}

// listen binds 127.0.0.1:port, simulating a classic instance owning the
// public port.
func listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// --- DecideLifecycle --------------------------------------------------------

func TestDecideLifecycle_ActiveInstanceAliveMeansRunning(t *testing.T) {
	resetHome(t)
	exe := selfBinary(t)
	state := &DeployState{
		AppName:    "appA",
		Mode:       ModeBlueGreen,
		PublicPort: 3000,
		ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: {Slot: SlotGreen, Status: "running", PID: os.Getpid(), BinaryPath: exe},
		},
	}
	d := DecideLifecycle(state)
	if d.Status != "running" {
		t.Fatalf("alive active instance must decide running, got %q", d.Status)
	}
	if d.PID != os.Getpid() {
		t.Fatalf("PID = %d, want %d", d.PID, os.Getpid())
	}
	if d.PublicPort != 3000 {
		t.Fatalf("PublicPort = %d, want 3000", d.PublicPort)
	}
}

func TestDecideLifecycle_DeadActiveInstanceMeansStopped(t *testing.T) {
	resetHome(t)
	state := &DeployState{
		AppName:    "appA",
		Mode:       ModeBlueGreen,
		PublicPort: 3000,
		ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: {Slot: SlotGreen, Status: "running", PID: 999999, BinaryPath: "/nonexistent/binary"},
		},
	}
	d := DecideLifecycle(state)
	if d.Status != "stopped" {
		t.Fatalf("dead active instance must decide stopped, got %q", d.Status)
	}
	if d.PID != 0 {
		t.Fatalf("PID = %d, want 0", d.PID)
	}
}

func TestDecideLifecycle_RecycledPIDWithWrongBinaryMeansStopped(t *testing.T) {
	resetHome(t)
	// PID alive (this test process) but recorded binary is something else —
	// must not count as the app running (PID-reuse guard).
	state := &DeployState{
		AppName:    "appA",
		Mode:       ModeBlueGreen,
		ActiveSlot: SlotBlue,
		Slots: map[string]*Instance{
			SlotBlue: {Slot: SlotBlue, Status: "running", PID: os.Getpid(), BinaryPath: "/bin/definitely-not-this-binary"},
		},
	}
	if d := DecideLifecycle(state); d.Status == "running" {
		t.Fatalf("PID reuse with mismatched binary must not report running")
	}
}

func TestDecideLifecycle_RollingFirstAliveReplica(t *testing.T) {
	resetHome(t)
	exe := selfBinary(t)
	state := &DeployState{
		AppName: "appA",
		Mode:    ModeRolling,
		Replicas: map[string]*Instance{
			"0": {Slot: "0", Status: "stopped", PID: 0},
			"1": {Slot: "1", Status: "running", PID: os.Getpid(), BinaryPath: exe},
		},
	}
	d := DecideLifecycle(state)
	if d.Status != "running" || d.PID != os.Getpid() {
		t.Fatalf("rolling: want running with first alive replica, got %+v", d)
	}
}

// --- TeardownDeployment ------------------------------------------------------

func TestTeardownDeployment_StopsLiveInstancesAndMarksStopped(t *testing.T) {
	resetHome(t)
	cmd, inst := spawnSleeper(t)

	state := &DeployState{
		AppName:    "appT",
		Mode:       ModeBlueGreen,
		PublicPort: 3000,
		ActiveSlot: SlotBlue,
		Slots: map[string]*Instance{
			SlotBlue:  inst,
			SlotGreen: {Slot: SlotGreen, Status: "stopped", PID: 999999},
		},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	log := &fakeLogger{}
	if err := TeardownDeployment(context.Background(), state, time.Second, log); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	// The real child must be gone.
	if pidAlive(cmd.Process.Pid) {
		t.Fatalf("sleeper pid %d still alive after teardown", cmd.Process.Pid)
	}
	reloaded := mustLoad(t, "appT")
	if reloaded.Slots[SlotBlue].Status != "stopped" || reloaded.Slots[SlotBlue].PID != 0 {
		t.Fatalf("blue slot = %+v, want stopped/pid 0", reloaded.Slots[SlotBlue])
	}
	// Historical metadata preserved for a later start.
	if reloaded.ActiveSlot != SlotBlue || reloaded.PublicPort != 3000 {
		t.Fatalf("metadata lost: %+v", reloaded)
	}
}

// --- StartDeployment + RestoreProxyRoute -------------------------------------

func TestStartDeployment_RestoresActiveSlotAndRoute(t *testing.T) {
	resetHome(t)

	fl := &httpLauncher{}
	defer fl.close()

	// Simulate a previously deployed app: active slot green with a recorded
	// binary. /bin/true exists everywhere and exits immediately — but the
	// launcher is faked, so the binary only needs to stat.
	state := &DeployState{
		AppName:    "appS",
		AppID:      "42",
		Mode:       ModeBlueGreen,
		PublicPort: 3000,
		ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: {Slot: SlotGreen, Status: "stopped", BinaryPath: "/bin/true"},
		},
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}

	if err := StartDeployment(context.Background(), state, StartOptions{Launcher: fl.Launch, ListenTimeout: 2 * time.Second, Logger: &fakeLogger{}}); err != nil {
		t.Fatalf("start: %v", err)
	}

	inst := mustLoad(t, "appS").Slots[SlotGreen]
	if inst.Status != "running" || inst.PID == 0 || inst.Port == 0 {
		t.Fatalf("green slot not restored: %+v", inst)
	}

	// Route restore: not enrolled -> Add with the fresh internal port.
	pc := &fakeProxyClient{alive: true}
	if err := RestoreProxyRoute(context.Background(), mustLoad(t, "appS"), pc, &fakeLogger{}); err != nil {
		t.Fatalf("restore route: %v", err)
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if len(pc.adds) != 1 || pc.addPort != 3000 {
		t.Fatalf("adds=%v addPort=%d, want one Add on 3000", pc.adds, pc.addPort)
	}
}

func TestStartDeployment_EnrolledAppSwitchesInsteadOfAdd(t *testing.T) {
	resetHome(t)

	fl := &httpLauncher{}
	defer fl.close()

	state := &DeployState{
		AppName:    "appS2",
		Mode:       ModeBlueGreen,
		PublicPort: 3000,
		ActiveSlot: SlotBlue,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped", BinaryPath: "/bin/true"},
			SlotGreen: {Slot: SlotGreen, Status: "stopped"},
		},
	}
	if err := StartDeployment(context.Background(), state, StartOptions{Launcher: fl.Launch, ListenTimeout: 2 * time.Second}); err != nil {
		t.Fatalf("start: %v", err)
	}

	pc := &fakeProxyClient{alive: true, enrolled: true}
	if err := RestoreProxyRoute(context.Background(), state, pc, &fakeLogger{}); err != nil {
		t.Fatalf("restore route: %v", err)
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if len(pc.switches) != 1 {
		t.Fatalf("enrolled app must Switch, got %d switches / %d adds", len(pc.switches), len(pc.adds))
	}
	if len(pc.adds) != 0 {
		t.Fatalf("enrolled app must not Add")
	}
}

func TestStartDeployment_MissingBinaryFails(t *testing.T) {
	resetHome(t)
	state := &DeployState{
		AppName:    "appS3",
		Mode:       ModeBlueGreen,
		ActiveSlot: SlotBlue,
		Slots: map[string]*Instance{
			SlotBlue: {Slot: SlotBlue, Status: "stopped", BinaryPath: "/nonexistent/app_bin"},
		},
	}
	if err := StartDeployment(context.Background(), state, StartOptions{}); err == nil {
		t.Fatalf("expected failure for missing recorded binary")
	}
}

// --- PortHandoff (classic → blue-green migration) -----------------------------

func TestBlueGreen_PortHandoffRunsWhenPublicPortOccupied(t *testing.T) {
	resetHome(t)

	// Occupy the public port the way a classic instance would.
	ln, err := listen("127.0.0.1:34567")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()

	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	var handoffCalls int
	var handoffMu sync.Mutex
	bg := &BlueGreen{
		AppName: "appM", AppID: "1", PublicPort: 34567,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
		PortHandoff: func(ctx context.Context, appName string, port int) error {
			handoffMu.Lock()
			handoffCalls++
			handoffMu.Unlock()
			ln.Close() // release the port, like stopping the classic instance
			return nil
		},
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy with handoff: %v", err)
	}
	handoffMu.Lock()
	defer handoffMu.Unlock()
	if handoffCalls != 1 {
		t.Fatalf("handoff calls = %d, want 1", handoffCalls)
	}
}

func TestBlueGreen_PortHandoffSkippedWhenPortFree(t *testing.T) {
	resetHome(t)

	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "appM2", AppID: "2", PublicPort: 34699, // nothing listens here
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
		PortHandoff: func(ctx context.Context, appName string, port int) error {
			t.Fatalf("handoff must not run when the public port is free")
			return nil
		},
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
}

func TestBlueGreen_PortHandoffFailureKeepsCandidateKilledAndAborts(t *testing.T) {
	resetHome(t)

	ln, err := listen("127.0.0.1:34777")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()

	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "appM3", AppID: "3", PublicPort: 34777,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
		PortHandoff: func(ctx context.Context, appName string, port int) error {
			return errHandoffFailed
		},
	}
	if err := bg.Deploy(context.Background()); err == nil {
		t.Fatalf("deploy must abort when handoff fails")
	}

	state := mustLoad(t, "appM3")
	// No slot became active; candidate recorded failed.
	if state.ActiveSlot != "" {
		t.Fatalf("active slot = %q, want empty", state.ActiveSlot)
	}
	inactive := state.InactiveSlot()
	if state.Slots[inactive] == nil || state.Slots[inactive].Status != "failed" {
		t.Fatalf("candidate slot = %+v, want failed", state.Slots[inactive])
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if len(pc.adds) != 0 {
		t.Fatalf("proxy must not be enrolled after handoff failure")
	}
}

var errHandoffFailed = &handoffError{}

type handoffError struct{}

func (*handoffError) Error() string { return "handoff failed" }
