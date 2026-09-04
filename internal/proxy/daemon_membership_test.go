package proxy

import (
	"context"
	"path/filepath"
	"testing"
)

// Bug regression for the duplicate-replica-0 incident: the rolling deploy
// historically sent the primary inside the backend set, and the daemon
// prepended the primary again when persisting — proxy-state.json ended up
// with the same replica twice and `phelix proxy status` showed N+1 backends.
// The daemon is the single point where the routing table is stored, so it
// must normalise membership to one entry per host no matter what a caller
// (old CLI included) sends.
func TestDaemon_BackendMembershipIdempotentAndUnique(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	prim := newTaggedBackend(t, "replica-0")
	defer prim.Close()
	b1 := newTaggedBackend(t, "replica-1")
	defer b1.Close()
	b2 := newTaggedBackend(t, "replica-2")
	defer b2.Close()

	socket := filepath.Join(home, "proxy.sock")
	d := NewDaemon(socket)
	if err := d.Run(); err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	c := NewClient(socket)
	ctx := context.Background()
	want := []Target{
		{Host: prim.addr(), Label: "replica-0"},
		{Host: b1.addr(), Label: "replica-1"},
		{Host: b2.addr(), Label: "replica-2"},
	}

	// Enrol with the primary duplicated inside the backend set (the historic
	// rolling-deploy wire shape), then reconcile repeatedly with the same and
	// with full sets including the primary.
	if err := c.Add(ctx, "app", 8091, want[0],
		Target{Host: prim.addr(), Label: "replica-0"},
		want[1], want[2]); err != nil {
		t.Fatalf("add: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := c.Switch(ctx, "app", want[0], want[1], want[2]); err != nil {
			t.Fatalf("switch %d: %v", i, err)
		}
		if err := c.Switch(ctx, "app", want[0],
			Target{Host: prim.addr(), Label: "replica-0"},
			want[1], want[2]); err != nil {
			t.Fatalf("switch-with-dup %d: %v", i, err)
		}
	}
	st, err := c.Status(ctx, "app")
	if err != nil || len(st) != 1 {
		t.Fatalf("status: %v %+v", err, st)
	}
	if st[0].Primary.Host != want[0].Host {
		t.Fatalf("primary = %+v, want %+v", st[0].Primary, want[0])
	}
	// The stored target set is primary-first, one entry per host: the
	// duplicated primary must have been normalised away, never persisted.
	got := st[0].Backends
	if len(got) != len(want) {
		t.Fatalf("backends = %+v, want exactly %d entries", got, len(want))
	}
	for i, t2 := range want {
		if got[i].Host != t2.Host || got[i].Label != t2.Label {
			t.Fatalf("backends[%d] = %+v, want %+v", i, got[i], t2)
		}
	}
}

// Version handshake: a current daemon reports its protocol version; clients
// use this in EnsureDaemon to replace daemons left over from older binaries.
func TestDaemon_ReportsProtocolVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	socket := filepath.Join(home, "proxy.sock")
	d := NewDaemon(socket)
	if err := d.Run(); err != nil {
		t.Fatalf("daemon run: %v", err)
	}
	t.Cleanup(func() { _ = d.Shutdown(context.Background()) })

	c := NewClient(socket)
	resp, err := c.Do(context.Background(), Request{Op: OpVersion})
	if err != nil || !resp.OK {
		t.Fatalf("version op: %v %+v", err, resp)
	}
	if resp.Version < proxyProtoVersion {
		t.Fatalf("daemon reports version %d, want >= %d", resp.Version, proxyProtoVersion)
	}
}
