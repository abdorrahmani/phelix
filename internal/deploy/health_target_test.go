package deploy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
)

// TestBlueGreen_ProbesCandidateInternalPortNotPublic (Test B): a decoy on the
// PUBLIC port answers 503 on the health path while the candidate answers 200
// on its own internal port. If the deploy health check probed the configured
// public URL instead of the candidate's listener, the deploy would abort.
func TestBlueGreen_ProbesCandidateInternalPortNotPublic(t *testing.T) {
	resetHome(t)

	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // /ping → 503 on the public port
	}))
	defer decoy.Close()
	decoyPort := portOf(decoy.URL)

	fl := &pathAwareLauncher{healthyPath: "/ping"}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "targetbg", AppID: "tb", PublicPort: decoyPort,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return pathHealth("/ping") },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy must probe the candidate's internal port, not the public URL: %v", err)
	}
}

// TestRolling_ProbesCandidateInternalPort (Test E): same contract for rolling —
// each replacement is validated against its own listener, never the public URL.
func TestRolling_ProbesCandidateInternalPort(t *testing.T) {
	resetHome(t)

	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer decoy.Close()

	fl := &pathAwareLauncher{healthyPath: "/ping"}
	defer fl.close()

	r := &Rolling{
		AppName: "targetroll", AppID: "tr", PublicPort: portOf(decoy.URL), Replicas: 2,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    &fakeProxyClient{alive: true},
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return pathHealth("/ping") },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := r.Deploy(context.Background()); err != nil {
		t.Fatalf("rolling must probe each candidate's internal port: %v", err)
	}
}

// dyingLauncher spawns a real process that exits almost immediately without
// binding anything — the "candidate crashed at boot" scenario.
func dyingLauncher(_ context.Context, _ string, _ []string) (Process, int, error) {
	ln, err := listenLocal()
	if err != nil {
		return nil, 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cmd := exec.Command("sh", "-c", "sleep 0.05; exit 3")
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	proc := &osProcess{cmd: cmd, pid: cmd.Process.Pid, doneCh: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		proc.reap(nil)
	}()
	return proc, port, nil
}

// TestBlueGreen_CandidateDeathFailsFastWithReason (§6): a candidate that dies
// at boot must abort the deploy immediately with the process-exit reason and
// the instance output tail, not burn the whole timeout as "unhealthy".
func TestBlueGreen_CandidateDeathFailsFastWithReason(t *testing.T) {
	resetHome(t)

	log := &fakeLogger{}
	bg := &BlueGreen{
		AppName: "dyingbg", AppID: "db", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       dyingLauncher,
		ProxyClient:    &fakeProxyClient{alive: true},
		Logger:         log,
		HealthProvider: func(string) *health.DeployTierConfig { return pathHealthShort("/ping") },
		GracePeriod:    200 * time.Millisecond,
	}

	start := time.Now()
	err := bg.Deploy(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("dead candidate must fail the deploy")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("candidate death must fail fast, took %s", elapsed)
	}
	// The typed cause must survive every wrap layer (errors.Is contract).
	if !errors.Is(err, health.ErrCandidateExited) {
		t.Fatalf("ErrCandidateExited must stay reachable through the chain, got: %v", err)
	}
	// And the human-facing chain must name the exit and the probe target.
	joined := err.Error() + chainText(err)
	if !strings.Contains(joined, "exited before becoming healthy") ||
		!strings.Contains(joined, "probe target: http://127.0.0.1:") {
		t.Fatalf("error chain missing death/target detail: %v", err)
	}
}

// chainText concatenates the messages of every wrapped cause.
func chainText(err error) string {
	var b strings.Builder
	for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
		b.WriteString("\n")
		b.WriteString(e.Error())
	}
	return b.String()
}

// TestDefaultLauncher_PortEnvWinsOverOverlay (§7): the candidate must be
// started with the internal port even when the app's env overlay also carries
// PORT (e.g. PORT=3000 stored in the env snapshot — the exact scenario that
// made candidates bind the busy public port and panic with AddrInUse).
func TestDefaultLauncher_PortEnvWinsOverOverlay(t *testing.T) {
	resetHome(t)

	dir := t.TempDir()
	bin := dir + "/envprobe"
	script := "#!/bin/sh\necho \"GOT_PORT=$PORT\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	proc, port, err := DefaultLauncher(context.Background(), bin, []string{"PORT=99999"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer func() { _ = proc.Signal(syscall.SIGKILL) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tail := instanceLogTail(bin, port, 4096); strings.Contains(tail, "GOT_PORT=") {
			if !strings.Contains(tail, "GOT_PORT="+itoa(port)) {
				t.Fatalf("candidate must receive the internal port, log tail: %q", tail)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("probe never wrote its port to the instance log")
}

// TestInstanceLogTail_BasicAndMissing: the helper returns redacted recent
// output for a live log and "" (not an error) when no log exists.
func TestInstanceLogTail_BasicAndMissing(t *testing.T) {
	resetHome(t)

	if tail := instanceLogTail("/nonexistent/binary", 4242, 1024); tail != "" {
		t.Fatalf("missing log must yield empty tail, got %q", tail)
	}

	// Write a log through the same path construction the logger uses.
	home, _ := os.UserHomeDir()
	dir := home + "/.phelix/logs"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bin := "/tmp/fakeapp"
	path := dir + "/deploy_fakeapp_4243.log"
	if err := os.WriteFile(path, []byte("panicked: token=abcdef1234567890\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	tail := instanceLogTail(bin, 4243, 1024)
	if !strings.Contains(tail, "panicked") {
		t.Fatalf("tail missing content: %q", tail)
	}
	if strings.Contains(tail, "abcdef1234567890") {
		t.Fatalf("tail must be redacted: %q", tail)
	}
}

// TestBlueGreen_StaleActiveSlotReEnrolls: after a classic build/rebuild (which
// stops the proxy-managed instance and starts one bound to the public port
// itself) deploy.json still says active="green" while the daemon has no
// enrolment. The deploy must consult the daemon and take the enrol (Add)
// path instead of Switching against an app the proxy does not know.
func TestBlueGreen_StaleActiveSlotReEnrolls(t *testing.T) {
	resetHome(t)

	pc := &fakeProxyClient{alive: true} // daemon: nothing enrolled
	fl := &pathAwareLauncher{healthyPath: "/ping"}
	defer fl.close()

	// Seed the stale state a classic run leaves behind.
	st, err := LoadOrInit("staleapp", ModeBlueGreen, 0)
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}
	st.ActiveSlot = "green"
	st.Slots = map[string]*Instance{
		SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
		SlotGreen: {Slot: SlotGreen, Status: "stopped", PID: 0},
	}
	if err := Store(st); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	bg := &BlueGreen{
		AppName: "staleapp", AppID: "stale", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return pathHealth("/ping") },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy with stale active slot must re-enroll, got: %v", err)
	}

	pc.mu.Lock()
	adds, switches := len(pc.adds), len(pc.switches)
	pc.mu.Unlock()
	if adds != 1 || switches != 0 {
		t.Fatalf("expected re-enrol via Add (1 add / 0 switches), got %d adds / %d switches", adds, switches)
	}
}

func listenLocal() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
