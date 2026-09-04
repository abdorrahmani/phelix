package proxy

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The rolling deploy path passes the primary inside the backend set; the
// proxy must normalise that to one entry per host instead of persisting a
// duplicated primary to proxy-state.json (the duplicate replica-0 regression).
func TestSetTargetDedupesPrimaryAndDuplicateHosts(t *testing.T) {
	p := New("dup", 8080, Target{Host: "127.0.0.1:1", Label: "replica-0"})
	backends := []Target{
		{Host: "127.0.0.1:1", Label: "replica-0"}, // primary repeated in backends
		{Host: "127.0.0.1:2", Label: "replica-1"},
		{Host: "127.0.0.1:2", Label: "replica-1-again"}, // duplicate host
		{Host: "127.0.0.1:3", Label: "replica-2"},
		{Host: "127.0.0.1:3", Label: "replica-2"},
	}
	p.SetTarget(Target{Host: "127.0.0.1:1", Label: "replica-0"}, backends...)
	got := p.Targets()
	want := []Target{
		{Host: "127.0.0.1:1", Label: "replica-0"},
		{Host: "127.0.0.1:2", Label: "replica-1"},
		{Host: "127.0.0.1:3", Label: "replica-2"},
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %+v, want %d unique backends", got, len(want))
	}
	for i := range want {
		if got[i].Host != want[i].Host || got[i].Label != want[i].Label {
			t.Fatalf("targets[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Idempotent: repeated reconciliation yields the same set.
	p.SetTarget(want[0], want...)
	if len(p.Targets()) != len(want) {
		t.Fatalf("repeated SetTarget changed the set: %+v", p.Targets())
	}
}

func TestReconcileBackendsDropsDeadBackends(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer live.Close()
	liveHost := live.Listener.Addr().String()

	// Dead backend: an unused loopback port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadHost := ln.Addr().String()
	ln.Close()
	time.Sleep(50 * time.Millisecond)

	primary := Target{Host: liveHost, Label: "replica-0"}
	backends := []Target{primary, {Host: deadHost, Label: "replica-1"}}

	gotPrimary, gotBackends := reconcileBackends(primary, backends)
	if len(gotBackends) != 1 || gotBackends[0].Host != liveHost {
		t.Fatalf("backends = %+v, want only the live one", gotBackends)
	}
	if gotPrimary.Host != liveHost {
		t.Fatalf("primary = %+v, want %s", gotPrimary, liveHost)
	}

	// Dead primary falls back to the first live backend.
	p2, b2 := reconcileBackends(Target{Host: deadHost, Label: "replica-0"}, backends)
	if p2.Host != liveHost {
		t.Fatalf("primary = %s, want fallback to %s", p2.Host, liveHost)
	}
	if len(b2) != 1 || b2[0].Host != liveHost {
		t.Fatalf("backends = %+v, want only the live one", b2)
	}

	// Nothing reachable: keep the persisted set untouched (instances may
	// still be restoring after a reboot).
	p3, b3 := reconcileBackends(Target{Host: deadHost}, []Target{{Host: deadHost}})
	if p3.Host != deadHost || len(b3) != 1 {
		t.Fatalf("all-dead reconcile mutated the set: %s %+v", p3.Host, b3)
	}
}
