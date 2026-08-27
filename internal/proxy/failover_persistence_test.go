package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// Regression coverage for the zero-downtime audit fixes on the proxy daemon:
// state persistence across restarts, round-robin distribution, and passive
// failover away from dead backends.

// Bug regression: killing/restarting the daemon used to drop every enrollment
// silently — all apps went dark until redeployed.
func TestDaemon_RestoresEnrollmentAfterRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	blue := newTaggedBackend(t, "blue")
	defer blue.Close()
	green := newTaggedBackend(t, "green")
	defer green.Close()

	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	publicPort := pln.Addr().(*net.TCPAddr).Port
	pln.Close()

	socket := filepath.Join(home, "proxy.sock")
	d1 := NewDaemon(socket)
	if err := d1.Run(); err != nil {
		t.Fatalf("daemon run 1: %v", err)
	}

	c := NewClient(socket)
	ctx := context.Background()
	if err := c.Add(ctx, "web", publicPort,
		Target{Host: blue.addr(), Label: "blue"},
		Target{Host: blue.addr(), Label: "blue"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := waitForBody(t, publicPort); got != "blue" {
		t.Fatalf("pre-restart body %q, want blue", got)
	}
	// Cut over to green so the restart must restore the NEWER target.
	if err := c.Switch(ctx, "web", Target{Host: green.addr(), Label: "green"}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if got := waitForBody(t, publicPort); got != "green" {
		t.Fatalf("pre-restart post-switch body %q, want green", got)
	}
	_ = d1.Shutdown(ctx) // simulates crash/stop of the daemon

	// Second daemon generation must restore web -> green immediately.
	d2 := NewDaemon(socket)
	if err := d2.Run(); err != nil {
		t.Fatalf("daemon run 2: %v", err)
	}
	t.Cleanup(func() { _ = d2.Shutdown(context.Background()) })

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping after restart: %v", err)
	}
	if got := waitForBody(t, publicPort); got != "green" {
		t.Fatalf("post-restart body %q, want green", got)
	}
	st, err := c.Status(ctx, "web")
	if err != nil || len(st) != 1 {
		t.Fatalf("status after restart: %v %+v", err, st)
	}
	if st[0].Primary.Label != "green" {
		t.Fatalf("restored primary=%+v, want green", st[0].Primary)
	}
	// Removal persists too: remove then restart again -> nothing restored.
	if err := c.Remove(ctx, "web"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	d3 := NewDaemon(socket)
	_ = d3.Run()
	t.Cleanup(func() { _ = d3.Shutdown(context.Background()) })
	if sts, _ := c.Status(ctx, ""); len(sts) != 0 {
		t.Fatalf("expected empty enrollment after remove+restart, got %+v", sts)
	}
}

// Bug regression / hardening: while several replicas are enrolled the proxy
// passively skips a target that refuses connections instead of serving it
// 502s until the next deploy switches membership.
func TestProxy_SkipsDeadTargetWhenOthersServe(t *testing.T) {
	live := newTaggedBackend(t, "live")
	defer live.Close()

	p := New("failover", 0, Target{Host: "127.0.0.1:1", Label: "dead"}) // closed port
	p.SetTarget(Target{Host: "127.0.0.1:1", Label: "dead"}, Target{Host: live.addr(), Label: "live"})

	srv := &http.Server{Handler: p.rp}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	port := ln.Addr().(*net.TCPAddr).Port
	deadline := time.Now().Add(5 * time.Second)
	var failures int
	for i := 0; i < 40 && time.Now().Before(deadline); i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			failures++
			continue
		}
		body := readAllString(t, resp)
		if body != "live" {
			t.Fatalf("got body %q, want live", body)
		}
	}
	if failures > 0 {
		t.Fatalf("%d requests failed while a healthy target existed (dead-target skip failed)", failures)
	}
}

// Rolling deploys should actually distribute across enrolled replicas.
func TestProxy_DistributesAcrossBackends(t *testing.T) {
	a := newTaggedBackend(t, "a")
	defer a.Close()
	b := newTaggedBackend(t, "b")
	defer b.Close()

	p := New("dist", 0, Target{Host: a.addr(), Label: "a"})
	p.SetTarget(Target{Host: a.addr(), Label: "a"}, Target{Host: a.addr(), Label: "a"}, Target{Host: b.addr(), Label: "b"})

	srv := &http.Server{Handler: p.rp}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	port := ln.Addr().(*net.TCPAddr).Port

	counts := map[string]int{}
	for i := 0; i < 60; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body := readAllString(t, resp)
		counts[body]++
	}
	// Round-robin over 3 slots where two map to backend "a": expect ~40/20.
	if counts["b"] == 0 || counts["a"] == 0 {
		t.Fatalf("no distribution observed: %v", counts)
	}
	if counts["a"] < counts["b"] {
		t.Fatalf("unexpected skew favoring b over a×2: %v", counts)
	}
}

func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}
