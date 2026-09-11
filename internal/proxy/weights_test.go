package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProxy_WeightedRoutingDistribution verifies the core canary primitive:
// with a 95/5 stable/canary split, exactly 95% of requests land on the stable
// backend and 5% on the canary, deterministically (smooth weighted
// round-robin, not random).
func TestProxy_WeightedRoutingDistribution(t *testing.T) {
	stable := newTaggedBackend(t, "stable")
	defer stable.Close()
	canary := newTaggedBackend(t, "canary")
	defer canary.Close()

	p := New("app", 0, Target{Host: stable.addr(), Label: "stable"})
	p.SetTarget(
		Target{Host: stable.addr(), Label: "stable", Weight: 95},
		Target{Host: canary.addr(), Label: "canary", Weight: 5},
	)

	const total = 200 // two full smooth-WRR cycles of total weight 100
	counts := map[string]int{}
	for i := 0; i < total; i++ {
		host := p.pickHealthyHost(p.loadState())
		if host == "" {
			t.Fatalf("pick %d: no host", i)
		}
		counts[host]++
	}
	if got := counts[stable.addr()]; got != 190 {
		t.Errorf("stable served %d/%d requests, want 190 (95%%)", got, total)
	}
	if got := counts[canary.addr()]; got != 10 {
		t.Errorf("canary served %d/%d requests, want 10 (5%%)", got, total)
	}
}

// TestProxy_WeightedRoutingViaHTTP drives the weighted split through the real
// Director so the weights flow all the way from SetTarget to the request path.
func TestProxy_WeightedRoutingViaHTTP(t *testing.T) {
	stable := newTaggedBackend(t, "stable")
	defer stable.Close()
	canary := newTaggedBackend(t, "canary")
	defer canary.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	p := New("app", port, Target{Host: stable.addr(), Label: "stable"})
	p.SetTarget(
		Target{Host: stable.addr(), Label: "stable", Weight: 80},
		Target{Host: canary.addr(), Label: "canary", Weight: 20},
	)
	srv := &http.Server{Handler: p.rp}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	counts := map[string]int{}
	const total = 100
	for i := 0; i < total; i++ {
		body := waitForBody(t, ln.Addr().(*net.TCPAddr).Port)
		counts[body]++
	}
	if got := counts["stable"]; got != 80 {
		t.Errorf("stable served %d, want 80", got)
	}
	if got := counts["canary"]; got != 20 {
		t.Errorf("canary served %d, want 20", got)
	}
}

// TestProxy_UnweightedSetStaysRoundRobin guards the rolling-deploy
// compatibility contract: a backend set without weights keeps the previous
// round-robin behavior (equal shares).
func TestProxy_UnweightedSetStaysRoundRobin(t *testing.T) {
	a := newTaggedBackend(t, "a")
	defer a.Close()
	b := newTaggedBackend(t, "b")
	defer b.Close()
	c := newTaggedBackend(t, "c")
	defer c.Close()

	p := New("app", 0, Target{Host: a.addr(), Label: "a"})
	p.SetTarget(
		Target{Host: a.addr(), Label: "a"},
		Target{Host: b.addr(), Label: "b"},
		Target{Host: c.addr(), Label: "c"},
	)

	counts := map[string]int{}
	const total = 90
	for i := 0; i < total; i++ {
		counts[p.pickHealthyHost(p.loadState())]++
	}
	for host, want := range map[string]int{a.addr(): 30, b.addr(): 30, c.addr(): 30} {
		if got := counts[host]; got != want {
			t.Errorf("backend %s served %d, want %d (equal round-robin)", host, got, want)
		}
	}
}

// TestProxy_WeightedSetSkipsUnhealthyCanary proves the invariant that a canary
// refusing connections receives no traffic — the stable backend takes all of
// it while the canary is down, without any reconfiguration.
func TestProxy_WeightedSetSkipsUnhealthyCanary(t *testing.T) {
	stable := newTaggedBackend(t, "stable")
	defer stable.Close()

	// A backend that is closed (connection refused) before routing starts.
	dead := newTaggedBackend(t, "dead")
	deadAddr := dead.addr()
	dead.Close()

	p := New("app", 0, Target{Host: stable.addr(), Label: "stable"})
	p.SetTarget(
		Target{Host: stable.addr(), Label: "stable", Weight: 95},
		Target{Host: deadAddr, Label: "canary", Weight: 5},
	)

	for i := 0; i < 50; i++ {
		if host := p.pickHealthyHost(p.loadState()); host != stable.addr() {
			t.Fatalf("pick %d routed to %q, want the healthy stable backend", i, host)
		}
	}
}

// TestProxy_BackendStats verifies the per-backend counters the canary
// verifier relies on: requests, 5xx errors and the latency buckets are
// attributed to the backend that actually served each request.
func TestProxy_BackendStats(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	stable := httptest.NewServer(mux)
	defer stable.Close()
	canary := newTaggedBackend(t, "canary")
	defer canary.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	p := New("app", port, Target{Host: stable.Listener.Addr().String()})
	p.SetTarget(
		Target{Host: stable.Listener.Addr().String(), Label: "stable", Weight: 50},
		Target{Host: canary.addr(), Label: "canary", Weight: 50},
	)
	srv := &http.Server{Handler: p.rp}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	// 6 requests through the weighted (50/50) split, all to /boom: only the
	// stable backend's handler answers 500 for it; the canary's catch-all
	// handler answers 200. With equal weights the smooth round-robin
	// alternates deterministically starting with the primary, so the stable
	// backend serves requests 0/2/4 and the canary 1/3/5.
	for i := 0; i < 6; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/boom", port))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	stats := map[string]BackendStat{}
	for _, s := range p.Stats() {
		stats[s.Host] = s
	}
	st := stats[stable.Listener.Addr().String()]
	if st.Requests != 3 {
		t.Errorf("stable requests = %d, want 3", st.Requests)
	}
	if st.Errors != 3 {
		t.Errorf("stable errors = %d, want 3 (its /boom answers 500)", st.Errors)
	}
	if st.Label != "stable" {
		t.Errorf("stable label = %q, want \"stable\"", st.Label)
	}
	ca := stats[canary.addr()]
	if ca.Requests != 3 {
		t.Errorf("canary requests = %d, want 3", ca.Requests)
	}
	if ca.Errors != 0 {
		t.Errorf("canary errors = %d, want 0", ca.Errors)
	}
	if len(ca.Buckets) != len(LatencyBucketBounds) {
		t.Fatalf("canary buckets = %d entries, want %d", len(ca.Buckets), len(LatencyBucketBounds))
	}
	var bucketTotal uint64
	for _, c := range ca.Buckets {
		bucketTotal += c
	}
	// Buckets are cumulative: the largest bucket must cover every request and
	// the total must not double-count.
	if ca.Buckets[len(ca.Buckets)-1] != uint64(ca.Requests) {
		t.Errorf("last bucket = %d, want %d (cumulative semantics)", ca.Buckets[len(ca.Buckets)-1], ca.Requests)
	}
	if bucketTotal == 0 {
		t.Error("no latency recorded in any bucket")
	}
}

// TestDaemon_StatsOpAndWeightPersistence drives the new control-surface
// pieces end-to-end over the unix socket: an Add carrying weights, a Stats
// query, and persistence of the weighted set across a daemon restart.
func TestDaemon_StatsOpAndWeightPersistence(t *testing.T) {
	stable := newTaggedBackend(t, "stable")
	defer stable.Close()
	canary := newTaggedBackend(t, "canary")
	defer canary.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	socket := filepath.Join(home, "proxy.sock")
	d := NewDaemon(socket)
	if err := d.Run(); err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	defer func() { _ = d.Shutdown(context.Background()) }()

	publicPort := freeTCPPort(t)
	prim := Target{Host: stable.addr(), Label: "stable", Weight: 95}
	back := Target{Host: canary.addr(), Label: "canary", Weight: 5}
	if err := d.EnrollApp("app", publicPort, prim, []Target{back}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// Serve one request so the stats op has something to report.
	client := NewClient(socket)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", publicPort))
	if err != nil {
		t.Fatalf("request via public port: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	stats, err := client.Stats(context.Background(), "app")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("stats: no backends reported")
	}
	var total int64
	for _, s := range stats {
		total += s.Requests
	}
	if total != 1 {
		t.Errorf("total requests across backends = %d, want 1", total)
	}

	// The weighted membership survives a restart: the persisted state file
	// carries the weights, and restoreState reinstates them.
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".phelix", "proxy-state.json"))
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	var pf struct {
		Apps []struct {
			AppName  string   `json:"app_name"`
			Primary  Target   `json:"primary"`
			Backends []Target `json:"backends"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatalf("decode persisted state: %v", err)
	}
	if len(pf.Apps) != 1 || pf.Apps[0].AppName != "app" {
		t.Fatalf("persisted apps = %+v, want one \"app\"", pf.Apps)
	}
	if pf.Apps[0].Primary.Weight != 95 {
		t.Errorf("persisted primary weight = %d, want 95", pf.Apps[0].Primary.Weight)
	}
	// The persisted backend list is the full target set (primary included,
	// restored through SetTarget's host-dedupe).
	if len(pf.Apps[0].Backends) != 2 ||
		pf.Apps[0].Backends[0].Weight != 95 || pf.Apps[0].Backends[1].Weight != 5 {
		t.Errorf("persisted backends = %+v, want stable(95) + canary(5)", pf.Apps[0].Backends)
	}

	d2 := NewDaemon(socket)
	if err := d2.Run(); err != nil {
		t.Fatalf("daemon restart: %v", err)
	}
	defer func() { _ = d2.Shutdown(context.Background()) }()
	p2 := d2.proxies["app"]
	if p2 == nil {
		t.Fatal("app not restored after daemon restart")
	}
	targets := p2.Targets()
	if len(targets) != 2 {
		t.Fatalf("restored targets = %d, want 2", len(targets))
	}
	if targets[0].Weight != 95 || targets[1].Weight != 5 {
		t.Errorf("restored weights = %d/%d, want 95/5", targets[0].Weight, targets[1].Weight)
	}
}

// TestProxy_UnknownOpReportsError makes sure a stats query against an app the
// daemon does not know answers an empty-but-ok result, not an error.
func TestDaemon_StatsUnknownAppIsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	socket := filepath.Join(home, "proxy.sock")
	d := NewDaemon(socket)
	if err := d.Run(); err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	defer func() { _ = d.Shutdown(context.Background()) }()

	client := NewClient(socket)
	stats, err := client.Stats(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("stats for unknown app: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("stats for unknown app = %+v, want empty", stats)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

var _ = strings.TrimSpace // keep strings imported for future assertions
