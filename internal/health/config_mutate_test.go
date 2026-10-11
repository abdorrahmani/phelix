package health

import (
	"fmt"
	"sync"
	"testing"
)

// TestConfigManagerMutateConcurrent proves the health lock serializes
// read-modify-write cycles: many concurrent adders (standing in for CLI + gRPC +
// daemon writers racing on the same health.json) each land their endpoint, with
// no lost update and no torn file.
func TestConfigManagerMutateConcurrent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cm, err := InitConfigManager()
	if err != nil {
		t.Fatalf("InitConfigManager: %v", err)
	}

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("ep-%d", i)
			_, err := cm.Mutate("app-1", "shop", func(c *AppHealthConfig) error {
				c.Endpoints[name] = &HealthCheckConfig{
					Name: name, URL: "http://localhost:3000/" + name,
					Interval: "10s", Retries: 3, ExpectedCodes: "200-299", Timeout: "10s",
				}
				return nil
			})
			if err != nil {
				t.Errorf("mutate %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got := cm.GetConfig("app-1")
	if got == nil {
		t.Fatal("config missing after concurrent mutations")
	}
	if len(got.Endpoints) != n {
		t.Fatalf("endpoints = %d, want %d (a concurrent write was lost)", len(got.Endpoints), n)
	}
	// Every endpoint got a stable id assigned.
	for name, ep := range got.Endpoints {
		if ep.ID == "" {
			t.Fatalf("endpoint %q has no id", name)
		}
	}
}

// TestConfigManagerMutateErrorIsNotPersisted confirms a closure error aborts the
// write (no auto-created empty config leaks to disk).
func TestConfigManagerMutateErrorIsNotPersisted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cm, err := InitConfigManager()
	if err != nil {
		t.Fatalf("InitConfigManager: %v", err)
	}
	sentinel := fmt.Errorf("boom")
	if _, err := cm.Mutate("app-1", "shop", func(*AppHealthConfig) error { return sentinel }); err != sentinel {
		t.Fatalf("Mutate err = %v, want sentinel", err)
	}
	if cfg := cm.GetConfig("app-1"); cfg != nil {
		t.Fatalf("a failed mutation must not persist a config, got %+v", cfg)
	}
}

// TestDaemonEventListenerRemoval is the regression guard for the watch leak: a
// listener added for a watch must be removable, so a watch session leaves no
// listener behind (what used to grow unbounded in the monitor process).
func TestDaemonEventListenerRemoval(t *testing.T) {
	d := &Daemon{eventListeners: make(map[int]EventListener)}

	id1 := d.AddEventListener(func(interface{}) {})
	id2 := d.AddEventListener(func(interface{}) {})
	if n := d.listenerCountForTest(); n != 2 {
		t.Fatalf("listener count = %d, want 2", n)
	}
	d.RemoveEventListener(id1)
	d.RemoveEventListener(id2)
	d.RemoveEventListener(99999) // unknown id is a no-op
	if n := d.listenerCountForTest(); n != 0 {
		t.Fatalf("listener count = %d, want 0 — a watch leaked its listener", n)
	}
}

func (d *Daemon) listenerCountForTest() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.eventListeners)
}
