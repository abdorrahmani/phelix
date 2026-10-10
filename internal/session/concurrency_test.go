package session

import (
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentCheckpointsAllLand proves the per-session lock serializes
// concurrent writers so no update is lost: N goroutines each checkpoint the
// same session and link a distinct deployment reference, and every one must be
// present afterwards. Run with -race to also prove there is no data race.
func TestConcurrentCheckpointsAllLand(t *testing.T) {
	isolate(t)
	s := mustCreate(t, CreateOpts{})
	const n = 40 // comfortably under MaxRefsPerKind and MaxEvents
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Checkpoint(s.SessionID, CheckpointOpts{
				Note:      fmt.Sprintf("step %d", i),
				Refs:      Refs{Deployments: []string{fmt.Sprintf("dep-%016x", i)}},
				ExpectRev: -1, // no optimistic check: writers serialize and all land
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent checkpoint failed: %v", err)
		}
	}
	got, err := Load(s.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rev != 1+n {
		t.Fatalf("rev = %d, want %d (every concurrent update must land exactly once)", got.Rev, 1+n)
	}
	if len(got.DeploymentIDs) != n {
		t.Fatalf("deployment refs = %d, want %d (no reference lost under concurrency)", len(got.DeploymentIDs), n)
	}
	if len(got.Events) != 1+n {
		t.Fatalf("events = %d, want %d (1 created + %d checkpoints)", len(got.Events), 1+n, n)
	}
}
