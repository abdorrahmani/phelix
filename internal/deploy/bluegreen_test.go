package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// fakeLogger captures log lines so tests can assert on them.
type fakeLogger struct {
	mu      sync.Mutex
	lines   []string
	warns   []string
	errors  []string
	success []string
}

func (l *fakeLogger) Stepf(f string, a ...any) { l.record("STEP", fmt.Sprintf(f, a...)) }
func (l *fakeLogger) Infof(f string, a ...any) { l.record("INFO", fmt.Sprintf(f, a...)) }
func (l *fakeLogger) Warnf(f string, a ...any) {
	l.mu.Lock()
	l.warns = append(l.warns, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}
func (l *fakeLogger) Successf(f string, a ...any) {
	l.mu.Lock()
	l.success = append(l.success, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}
func (l *fakeLogger) Errorf(f string, a ...any) {
	l.mu.Lock()
	l.errors = append(l.errors, fmt.Sprintf(f, a...))
	l.mu.Unlock()
}

func (l *fakeLogger) record(prefix, line string) {
	l.mu.Lock()
	l.lines = append(l.lines, prefix+": "+line)
	l.mu.Unlock()
}

// fakeProxyClient records calls to Add/Switch/Remove.
type fakeProxyClient struct {
	mu       sync.Mutex
	pinged   bool
	adds     []string
	switches []proxy.Target
	removes  []string
	addPort  int
	alive    bool
}

func (f *fakeProxyClient) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinged = true
	if !f.alive {
		return errors.New("daemon not running")
	}
	return nil
}
func (f *fakeProxyClient) Add(_ context.Context, name string, port int, _ proxy.Target, _ ...proxy.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, name)
	f.addPort = port
	return nil
}
func (f *fakeProxyClient) Switch(_ context.Context, _ string, prim proxy.Target, _ ...proxy.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switches = append(f.switches, prim)
	return nil
}
func (f *fakeProxyClient) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, name)
	return nil
}
func (f *fakeProxyClient) Status(context.Context, string) ([]proxy.AppStatus, error) {
	return nil, nil
}

// httpLauncher starts an httptest server as a stand-in for a healthy instance
// and returns its port plus a Process (the test process, which stays alive).
type httpLauncher struct {
	servers []*httptest.Server
}

func (fl *httpLauncher) Launch(_ context.Context, _ string, _ []string) (Process, int, error) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	fl.servers = append(fl.servers, srv)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	return &selfProc{pid: os.Getpid()}, port, nil
}

func (fl *httpLauncher) close() {
	for _, s := range fl.servers {
		s.Close()
	}
}

// selfProc is a Process backed by the current test process.
type selfProc struct{ pid int }

func (s *selfProc) PID() int               { return s.pid }
func (s *selfProc) Signal(os.Signal) error { return nil }
func (s *selfProc) Kill() error            { return nil }
func (s *selfProc) Wait() error            { return nil }

// closedLauncher returns a port with nothing listening, so health fails.
type closedLauncher struct{}

func (closedLauncher) Launch(_ context.Context, _ string, _ []string) (Process, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return &selfProc{pid: 1}, port, nil
}

// stubBuilder returns a canned binary path without doing any real build.
func stubBuilder(path string) Builder {
	return func(context.Context, string, string, []string) (string, error) {
		return path, nil
	}
}

// fastHealth is a tier config that makes WaitForHealthy complete quickly.
func fastHealth() *health.DeployTierConfig {
	return &health.DeployTierConfig{Interval: "5ms", Retries: 2, Timeout: "3s"}
}

// fastHealthShortTimeout is a tier config whose overall timeout is tiny, used
// for the unhealthy-instance failure test.
func fastHealthShortTimeout() *health.DeployTierConfig {
	return &health.DeployTierConfig{Interval: "5ms", Retries: 2, Timeout: "400ms"}
}

// --- blue-green tests ------------------------------------------------------

func TestBlueGreen_FirstDeploy_HappyPath(t *testing.T) {
	resetHome(t)

	log := &fakeLogger{}
	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "appA", AppID: "1", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         log,
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
	}

	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	// First deploy enrols the app.
	pc.mu.Lock()
	addCount := len(pc.adds)
	switchCount := len(pc.switches)
	pc.mu.Unlock()
	if addCount != 1 {
		t.Fatalf("expected one Add, got %d", addCount)
	}
	if switchCount != 0 {
		t.Fatalf("first deploy should not Switch, got %d", switchCount)
	}

	// State persisted with active slot.
	s, err := Load("appA")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if s.ActiveSlot == "" {
		t.Fatalf("expected active slot set after first deploy")
	}
	if got := s.Slots[s.ActiveSlot].Status; got != "running" {
		t.Fatalf("active slot status = %q, want running", got)
	}
	// Tier 2 fallback (httptest responds) should have warned.
	log.mu.Lock()
	warnCount := len(log.warns)
	log.mu.Unlock()
	if warnCount == 0 {
		t.Fatalf("expected a Tier 2/3 warning, got none")
	}
}

func TestBlueGreen_SecondDeploy_SwitchesAndStopsOld(t *testing.T) {
	resetHome(t)

	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "appB", AppID: "2", PublicPort: 0,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealth() },
		GracePeriod:    200 * time.Millisecond,
	}

	// First deploy.
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	firstActive := mustLoad(t, "appB").ActiveSlot
	// Patch the old instance to a nonexistent PID so step 8's stopByPID is a
	// no-op (the real instance is the test process; we don't want to kill it).
	st := mustLoad(t, "appB")
	st.Slots[firstActive].PID = 999999
	if err := Store(st); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Second deploy.
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("second deploy: %v", err)
	}
	secondState := mustLoad(t, "appB")
	secondActive := secondState.ActiveSlot
	if firstActive == secondActive {
		t.Fatalf("expected active slot to change: was %s, still %s", firstActive, secondActive)
	}

	pc.mu.Lock()
	switches := append([]proxy.Target{}, pc.switches...)
	pc.mu.Unlock()
	// Exactly one Switch on the second deploy, target = new slot.
	if len(switches) != 1 {
		t.Fatalf("expected 1 switch, got %d", len(switches))
	}
	if switches[0].Label != secondActive {
		t.Fatalf("switch target = %q, want %q", switches[0].Label, secondActive)
	}
	// Old slot marked stopped.
	if st := mustLoad(t, "appB"); st.Slots[firstActive].Status != "stopped" {
		t.Fatalf("old slot status = %q, want stopped", st.Slots[firstActive].Status)
	}
}

func TestBlueGreen_UnhealthyInstance_AbortsLeavesActiveUntouched(t *testing.T) {
	resetHome(t)

	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "appC", AppID: "3", PublicPort: 0,
		Builder: stubBuilder("/bin/true"),
		Launcher: func(ctx context.Context, _ string, _ []string) (Process, int, error) {
			return fl.Launch(ctx, "", nil)
		},
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastHealthShortTimeout() },
		GracePeriod:    200 * time.Millisecond,
	}
	// First deploy succeeds (healthy launcher).
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	before := mustLoad(t, "appC")
	firstActive := before.ActiveSlot
	firstPID := before.Slots[firstActive].PID
	pc.mu.Lock()
	switchesBefore := len(pc.switches)
	pc.mu.Unlock()

	// Swap to a launcher whose instance never answers -> second deploy fails.
	bg.Launcher = closedLauncher{}.Launch
	if err := bg.Deploy(context.Background()); err == nil {
		t.Fatalf("expected second deploy to fail (unhealthy instance)")
	}

	after := mustLoad(t, "appC")
	// Active slot untouched: same slot, same PID.
	if after.ActiveSlot != firstActive {
		t.Fatalf("active slot changed to %q, expected unchanged %q", after.ActiveSlot, firstActive)
	}
	if after.Slots[firstActive].PID != firstPID {
		t.Fatalf("active instance PID changed: was %d now %d", firstPID, after.Slots[firstActive].PID)
	}
	pc.mu.Lock()
	switchesAfter := len(pc.switches)
	pc.mu.Unlock()
	// No additional Switch call: traffic not re-pointed to the failed instance.
	if switchesAfter != switchesBefore {
		t.Fatalf("expected no new switch on failure, got %d (before %d)", switchesAfter, switchesBefore)
	}
	// Failed slot marked failed.
	other := after.InactiveSlot()
	if after.Slots[other].Status != "failed" {
		t.Fatalf("failed slot status = %q, want failed", after.Slots[other].Status)
	}
}

// --- graceful-stop tests ---------------------------------------------------

// TestGracefulStop_SIGTERMRespectedWithinGrace: a process that exits promptly
// on SIGTERM must NOT be force-killed.
func TestGracefulStop_SIGTERMRespectedWithinGrace(t *testing.T) {
	fp := newFakeProc(50 * time.Millisecond)

	report, err := GracefulStop(context.Background(), fp, 2*time.Second, 0)
	if err != nil {
		t.Fatalf("GracefulStop: %v", err)
	}
	if report.ForceKilled {
		t.Fatalf("process was force-killed despite exiting within grace")
	}
	if !report.Exited {
		t.Fatalf("expected Exited=true")
	}
	if fp.killed.Load() {
		t.Fatalf("Kill should not have been called")
	}
}

// TestGracefulStop_ForceKillsAfterGrace: a process that ignores SIGTERM must
// be force-killed once the grace period elapses.
func TestGracefulStop_ForceKillsAfterGrace(t *testing.T) {
	fp := newFakeProc(0) // 0 = never exit on SIGTERM
	fp.killExits = true  // Kill() makes Wait() return

	report, err := GracefulStop(context.Background(), fp, 100*time.Millisecond, 0)
	if err != nil {
		t.Fatalf("GracefulStop: %v", err)
	}
	if !report.ForceKilled {
		t.Fatalf("expected force-kill after grace")
	}
	if !fp.killed.Load() {
		t.Fatalf("Kill was not called")
	}
	if report.Elapsed > time.Second {
		t.Fatalf("GracefulStop took too long: %v", report.Elapsed)
	}
}

// fakeProc is a fully in-memory Process for the graceful-stop tests.
type fakeProc struct {
	pid       int
	signaled  atomic.Bool
	killed    atomic.Bool
	exitAfter time.Duration // delay before Wait returns after Signal (0 = never)
	exitCh    chan struct{}
	killExits bool
	killErr   error // optional: Kill returns this error instead of nil
}

func newFakeProc(exitAfter time.Duration) *fakeProc {
	return &fakeProc{pid: 1234, exitAfter: exitAfter, exitCh: make(chan struct{}, 1)}
}

func (f *fakeProc) PID() int { return f.pid }
func (f *fakeProc) Signal(os.Signal) error {
	f.signaled.Store(true)
	if f.exitAfter > 0 {
		go func() {
			time.Sleep(f.exitAfter)
			select {
			case f.exitCh <- struct{}{}:
			default:
			}
		}()
	}
	return nil
}
func (f *fakeProc) Kill() error {
	f.killed.Store(true)
	if f.killErr != nil {
		return f.killErr
	}
	if f.killExits {
		select {
		case f.exitCh <- struct{}{}:
		default:
		}
	}
	return nil
}
func (f *fakeProc) Wait() error {
	<-f.exitCh
	return nil
}

// --- shared helpers --------------------------------------------------------

// resetHome redirects HOME to a temp dir so the real ~/.phelix is untouched.
func resetHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func mustLoad(t *testing.T, name string) *DeployState {
	t.Helper()
	s, err := Load(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return s
}
