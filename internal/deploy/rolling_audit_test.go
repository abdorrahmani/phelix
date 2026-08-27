package deploy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/proxy"
)

// Regression coverage for the zero-downtime audit fixes (rolling side).

// multiLauncher hands out successive healthy instances and keeps every server
// alive so previous "generations" keep serving through replacements.
type multiLauncher struct {
	mu      sync.Mutex
	servers []*httptest.Server
	last    *countingProc
}

func (m *multiLauncher) Launch(_ context.Context, _ string, _ []string) (Process, int, error) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servers = append(m.servers, srv)
	m.last = &countingProc{selfProc: selfProc{pid: os.Getpid()}}
	return m.last, port, nil
}

func (m *multiLauncher) lastServerPort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.servers[len(m.servers)-1]
	return s.Listener.Addr().(*net.TCPAddr).Port
}

func (m *multiLauncher) lastProc() *countingProc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

func (m *multiLauncher) close() {
	for _, s := range m.servers {
		s.Close()
	}
}

// auditedClient extends fakeProxyClient with Status rows (rolling's enrolled
// detection) and a snapshot of every membership update it sees.
type auditedClient struct {
	inner      *fakeProxyClient
	statusRows []proxy.AppStatus

	mu      sync.Mutex
	swapped [][]proxy.Target // backends per Add/Switch call, in order
}

func (a *auditedClient) Ping(ctx context.Context) error { return a.inner.Ping(ctx) }

func (a *auditedClient) Add(ctx context.Context, name string, port int, prim proxy.Target, b ...proxy.Target) error {
	a.record(append([]proxy.Target{prim}, b...))
	return a.inner.Add(ctx, name, port, prim, b...)
}

func (a *auditedClient) Switch(_ context.Context, _ string, prim proxy.Target, backends ...proxy.Target) error {
	a.record(append([]proxy.Target{prim}, backends...))
	return nil
}

func (a *auditedClient) Remove(ctx context.Context, name string) error {
	return a.inner.Remove(ctx, name)
}

func (a *auditedClient) Status(_ context.Context, _ string) ([]proxy.AppStatus, error) {
	if len(a.statusRows) == 0 {
		return nil, nil
	}
	return a.statusRows, nil
}

func (a *auditedClient) record(t []proxy.Target) {
	a.mu.Lock()
	a.swapped = append(a.swapped, t)
	a.mu.Unlock()
}

// seededReplica fills state.Replicas[key] as if deployed by an earlier run.
func seededReplica(state *DeployState, key string, port, version int) {
	state.Replicas[key] = &Instance{
		Slot: key, PID: os.Getpid(), Port: port,
		BinaryPath: "/bin/true", StartedAt: time.Now().Add(-time.Hour),
		Status: "running", Version: version,
	}
}

// Bug regression: the first-ever rolling deploy never called Add, leaving the
// app unenrolled while Switch warned non-fatally — silent total outage.
func TestRolling_FirstDeployEnrolls(t *testing.T) {
	resetHome(t)
	fl := &multiLauncher{}
	defer fl.close()
	pc := &fakeProxyClient{alive: true}

	r := &Rolling{
		AppName: "roll-first", AppID: "rf", PublicPort: 8110, Replicas: 2,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    pc,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastCfg() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := r.Deploy(context.Background()); err != nil {
		t.Fatalf("first rolling deploy: %v", err)
	}
	pc.mu.Lock()
	adds, switches := len(pc.adds), len(pc.switches)
	pc.mu.Unlock()
	if adds != 1 || switches != 1 {
		t.Fatalf("adds=%d switches=%d, want 1/1", adds, switches)
	}
	st := mustLoad(t, "roll-first")
	for k, inst := range st.Replicas {
		if inst.Status != "running" || inst.Port == 0 {
			t.Fatalf("replica %s not running: %+v", k, inst)
		}
	}
}

// Bug regression: the old flow stopped each replica BEFORE its replacement
// was healthy, so every replacement opened a routing hole (the proxy kept
// dialling a dead port until the post-health switch). Each membership update
// must therefore expose full desired capacity.
func TestRolling_MembershipKeepsNBackendsDuringReplace(t *testing.T) {
	resetHome(t)
	fl := &multiLauncher{}
	defer fl.close()

	state := &DeployState{
		AppName: "roll-holes", AppID: "rh", Mode: ModeRolling, PublicPort: 8112,
		ActiveVersion: 1,
		Replicas:      map[string]*Instance{},
	}
	for _, k := range []string{"0", "1", "2"} {
		fl.Launch(context.Background(), "", nil) // live backend of generation zero
		seededReplica(state, k, fl.lastServerPort(), 1)
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}

	ac := &auditedClient{
		inner:      &fakeProxyClient{alive: true},
		statusRows: []proxy.AppStatus{{AppName: "roll-holes"}},
	}
	r := &Rolling{
		AppName: "roll-holes", AppID: "rh", PublicPort: 8112, Replicas: 3,
		Builder:        stubBuilder("/bin/true"),
		Launcher:       fl.Launch,
		ProxyClient:    ac,
		Logger:         &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig { return fastCfg() },
		GracePeriod:    200 * time.Millisecond,
	}
	if err := r.Deploy(context.Background()); err != nil {
		t.Fatalf("rolling deploy: %v", err)
	}

	ac.mu.Lock()
	snaps := ac.swapped
	ac.mu.Unlock()
	if len(snaps) != 3 {
		t.Fatalf("membership updates=%d, want 3", len(snaps))
	}
	for i, members := range snaps {
		if len(members) < 3 {
			t.Fatalf("update #%d exposed only %d backends (capacity hole): %+v",
				i, len(members), members)
		}
	}
	st := mustLoad(t, "roll-holes")
	for k, inst := range st.Replicas {
		if inst.Status != "running" || inst.Port == 0 {
			t.Fatalf("replica %s wrong end state: %+v", k, inst)
		}
	}
}
