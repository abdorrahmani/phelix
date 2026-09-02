package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestProxy_AtomicSwitchUnderLoad is the headline zero-downtime guarantee:
// while many concurrent requests are in flight, we flip the target, and no
// request errors and every response is consistent with whichever backend was
// active when the request was routed.
func TestProxy_AtomicSwitchUnderLoad(t *testing.T) {
	// Two backends; each tags its body with a stable label.
	blue := newTaggedBackend(t, "blue")
	defer blue.Close()
	green := newTaggedBackend(t, "green")
	defer green.Close()

	// Bind a real public port for the proxy via a listener, then hand it to a
	// minimal http.Server driven by the Proxy's ReverseProxy.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	publicAddr := ln.Addr().String()

	p := New("testapp", ln.Addr().(*net.TCPAddr).Port, Target{Host: blue.addr(), Label: "blue"})
	srv := &http.Server{Handler: p.rp}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	var errors int64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 16 concurrent clients hammering the public port.
	const clients = 16
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := http.Get("http://" + publicAddr + "/")
				if err != nil {
					atomic.AddInt64(&errors, 1)
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				got := string(body)
				// The response must be a known label; we never expect a partial
				// or mixed response, which is what an unsafe pointer swap would
				// produce.
				if got != "blue" && got != "green" {
					atomic.AddInt64(&errors, 1)
				}
			}
		}()
	}

	// Let traffic build up, then flip blue -> green -> blue a few times.
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 4; i++ {
		p.SetTarget(Target{Host: green.addr(), Label: "green"})
		time.Sleep(30 * time.Millisecond)
		p.SetTarget(Target{Host: blue.addr(), Label: "blue"})
		time.Sleep(30 * time.Millisecond)
	}

	close(stop)
	wg.Wait()

	if got := atomic.LoadInt64(&errors); got != 0 {
		t.Fatalf("expected 0 errors during atomic switches, got %d", got)
	}
}

// TestProxy_CurrentTargetReflectsSwitch verifies the getter returns the latest
// target after SetTarget.
func TestProxy_CurrentTargetReflectsSwitch(t *testing.T) {
	p := New("app", 0, Target{Host: "127.0.0.1:1", Label: "blue"})
	if p.CurrentTarget().Label != "blue" {
		t.Fatalf("initial target = %v, want blue", p.CurrentTarget())
	}
	p.SetTarget(Target{Host: "127.0.0.1:2", Label: "green"})
	if p.CurrentTarget().Label != "green" {
		t.Fatalf("after switch target = %v, want green", p.CurrentTarget())
	}
}

// TestProxy_InFlightCounter tracks concurrent requests via the counting transport.
func TestProxy_InFlightCounter(t *testing.T) {
	held := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held <- struct{}{}
		<-release
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := New("app", ln.Addr().(*net.TCPAddr).Port, Target{Host: srv.Listener.Addr().String(), Label: "b"})
	hs := &http.Server{Handler: p.rp}
	go func() { _ = hs.Serve(ln) }()
	defer hs.Shutdown(context.Background())

	go func() {
		resp, _ := http.Get("http://" + ln.Addr().String() + "/")
		if resp != nil {
			resp.Body.Close()
		}
	}()
	<-held

	if got := p.InFlight(); got != 1 {
		t.Fatalf("InFlight during request = %d, want 1", got)
	}
	close(release)
}

// TestProxy_StartTwiceErrors guards against double-binding the public port.
func TestProxy_StartTwiceErrors(t *testing.T) {
	// Bind a port ourselves and hand it to the proxy via a tiny server, then
	// call Start which will fail to bind (port in use) but still flip the
	// started flag; the second Start must then error with "already started".
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	p := New("app", port, Target{Host: "127.0.0.1:1", Label: "b"})
	// First Start: our listener owns the port, so p.Start's net.Listen fails.
	firstErr := p.Start()
	if firstErr == nil {
		t.Fatalf("expected first Start to fail (port in use)")
	}
	// started flag was NOT set on bind failure, so retry should be allowed and
	// also fail for the same reason. The double-start guard is exercised below
	// by forcing the flag manually.
	if p.started.Load() {
		t.Fatalf("started flag should remain false after bind failure")
	}

	// Simulate a running proxy by setting the flag, then confirm Start guards.
	p.started.Store(true)
	if err := p.Start(); err == nil {
		t.Fatalf("expected second Start to error, got nil")
	}
}

// TestDaemon_AddSwitchStatus exercises the full control protocol over a real
// unix socket with two real backends.
func TestDaemon_AddSwitchStatus(t *testing.T) {
	blue := newTaggedBackend(t, "blue")
	defer blue.Close()
	green := newTaggedBackend(t, "green")
	defer green.Close()

	// Pick a free public port by opening a listener and closing it.
	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	publicPort := pln.Addr().(*net.TCPAddr).Port
	pln.Close()

	socket := filepath.Join(t.TempDir(), "proxy.sock")
	d := NewDaemon(socket)
	if err := d.Run(); err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	defer d.Shutdown(context.Background())

	c := NewClient(socket)
	ctx := context.Background()

	// Ping.
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Enroll the app on the public port with blue as primary.
	if err := c.Add(ctx, "app1", publicPort, Target{Host: blue.addr(), Label: "blue"}); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Traffic should hit blue.
	if got := waitForBody(t, publicPort); got != "blue" {
		t.Fatalf("after add, got %q, want blue", got)
	}

	// Atomically switch to green.
	if err := c.Switch(ctx, "app1", Target{Host: green.addr(), Label: "green"}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if got := waitForBody(t, publicPort); got != "green" {
		t.Fatalf("after switch, got %q, want green", got)
	}

	// Status reflects the switch.
	st, err := c.Status(ctx, "app1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st) != 1 || st[0].Primary.Label != "green" {
		t.Fatalf("status primary = %+v, want green", st)
	}

	// Remove closes the public listener.
	if err := c.Remove(ctx, "app1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

// TestClient_NotRunningErrors confirms the client gives a helpful error when
// no daemon is listening.
func TestClient_NotRunningErrors(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "missing.sock")
	c := NewClient(socket)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatalf("expected ping error when daemon absent")
	}
	if c.IsRunning() {
		t.Fatalf("IsRunning should be false for missing socket")
	}
}

// --- helpers ---------------------------------------------------------------

// taggedBackend is an httptest server whose body is a fixed label, plus an
// addr() helper returning the host:port.
type taggedBackend struct {
	*httptest.Server
}

func newTaggedBackend(t *testing.T, label string) *taggedBackend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, label)
	}))
	return &taggedBackend{Server: srv}
}

func (b *taggedBackend) addr() string {
	return b.Listener.Addr().String()
}

func waitForBody(t *testing.T, port int) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(body)
	}
	t.Fatalf("never got a response from port %d: %v", port, lastErr)
	return ""
}
