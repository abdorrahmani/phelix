package app

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// TestLoadStateConcurrentWithLockedReaders reproduces the daemon crash class
// the autoscaling remote commands hit: an unlocked LoadState (map writer)
// racing a ListApplications tick (locked LoadState) on the shared app map.
// LoadState must serialize on the manager lock so every caller is safe by
// construction. Run with -race: without the lock this test fatals on
// concurrent map writes.
func TestLoadStateConcurrentWithLockedReaders(t *testing.T) {
	isolateStateFile(t)
	seed := map[string]map[string]any{
		"app-a": {"id": "app-a", "name": "app-a", "pid": 999991, "status": "running"},
		"app-b": {"id": "app-b", "name": "app-b", "pid": 999992, "status": "running"},
		"app-c": {"id": "app-c", "name": "app-c", "pid": 999993, "status": "running"},
	}
	blob, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(stateFile, blob, 0o644); err != nil {
		t.Fatalf("seed state file: %v", err)
	}
	if err := Manager.LoadState(); err != nil {
		t.Fatalf("initial LoadState: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	fail := func(what string, err error) {
		t.Errorf("%s: %v", what, err)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if err := Manager.LoadState(); err != nil {
						fail("LoadState", err)
						return
					}
				}
			}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = Manager.ListApplications()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := Manager.StatusApplication("app-a"); err != nil {
					fail("StatusApplication", err)
					return
				}
			}
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
