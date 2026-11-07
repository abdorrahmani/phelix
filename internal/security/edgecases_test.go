package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestInventoryRust_RootPackageWorkspace pins the implicit-member rule: a
// workspace root carrying its own [package] counts as a member, so its
// dependencies are direct, its lockfile entry is not misread as a path
// dependency, and reachability is seeded from it.
func TestInventoryRust_RootPackageWorkspace(t *testing.T) {
	inv, err := InventoryRust("testdata/rustroot")
	if err != nil {
		t.Fatalf("InventoryRust: %v", err)
	}
	for _, mod := range inv.Modules {
		if mod.Name == "rootcrate" {
			t.Fatalf("the root crate must not appear as a dependency: %+v", mod)
		}
		if mod.Name == "leafcrate" && !mod.Reachable {
			t.Fatalf("leafcrate is reachable through the root: %+v", mod)
		}
		if mod.Name == "depofroot" {
			if !mod.Direct {
				t.Fatalf("dependencies of the root crate are direct: %+v", mod)
			}
			if len(mod.DeclaredBy) == 0 || mod.DeclaredBy[0] != "rootcrate" {
				t.Fatalf("root crate must be listed as declarer: %+v", mod.DeclaredBy)
			}
		}
	}
}

// TestOSVClient_NoNestedRetries pins the retry ceiling: two rate-limited
// responses followed by success cost exactly three HTTP attempts, not a
// multiplicative cascade.
func TestOSVClient_NoNestedRetries(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= 2 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":"GO-2024-1001"}`))
	}))
	defer server.Close()

	client := &OSVClient{BaseURL: server.URL}
	adv, err := client.FetchAdvisory(context.Background(), "GO-2024-1001")
	if err != nil || adv == nil {
		t.Fatalf("FetchAdvisory: %v %v", adv, err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Fatalf("exactly three attempts expected, got %d", got)
	}
}

// TestPrefetchSource_PartialBatchKeepsFetchedIDs pins the partial-batch
// fix: when a later batch fails, the identities of successful batches
// stay resolved and coverage reports the shortfall.
func TestPrefetchSource_PartialBatchKeepsFetchedIDs(t *testing.T) {
	prevSize := osvBatchSize
	osvBatchSize = 1
	defer func() { osvBatchSize = prevSize }()

	var request atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if request.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"results":[{"vulns":[{"id":"GO-2024-1001"}]}]}`))
			return
		}
		http.Error(w, "upstream failure", http.StatusInternalServerError)
	}))
	defer server.Close()

	cache := NewAdvisoryCache(t.TempDir())
	source := NewPrefetchSource(cache, &OSVClient{BaseURL: server.URL})
	queries := []OSVQuery{
		{Ecosystem: EcosystemGo, Name: "example.com/one"},
		{Ecosystem: EcosystemGo, Name: "example.com/two"},
		{Ecosystem: EcosystemGo, Name: "example.com/three"},
	}
	if err := source.Prefetch(context.Background(), queries); err != nil {
		t.Fatalf("Prefetch never fails: %v", err)
	}
	if !source.NetworkFailed() {
		t.Fatal("the failed batch must mark the source degraded")
	}
	resolved, total := source.Coverage()
	if resolved != 1 || total != 3 {
		t.Fatalf("coverage must keep the fetched batch: resolved=%d total=%d", resolved, total)
	}
	entries, err := source.Advisories(context.Background(), EcosystemGo, "example.com/one")
	if err != nil || len(entries) != 0 {
		t.Fatalf("the resolved query maps to its (empty) id set: %v %v", entries, err)
	}
}

// TestAdvisoryCache_ConcurrentQuarantineNeverFails pins the race fix:
// concurrent readers of one corrupt entry quarantine it best-effort and
// every reader observes an absent entry, never an error.
func TestAdvisoryCache_ConcurrentQuarantineNeverFails(t *testing.T) {
	dir := t.TempDir()
	cache := NewAdvisoryCache(dir)
	path := cache.path("GO-2024-1001")
	if err := writeFileAtomic(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			adv, stale, err := cache.Get("GO-2024-1001")
			if err != nil || adv != nil || stale {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent quarantine must be invisible to readers: %v", err)
	}
}

// TestAdvisoryCache_PruneRemovesCorruptFiles ensures quarantine leftovers
// do not accumulate forever.
func TestAdvisoryCache_PruneRemovesCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	cache := NewAdvisoryCache(dir)
	corrupt := cache.path("GO-2024-1001") + ".corrupt.20260101T000000"
	if err := writeFileAtomic(corrupt, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(corrupt, old, old); err != nil {
		t.Fatal(err)
	}
	cache.Prune(AdvisoryCachePruneAge)
	if _, err := os.Stat(corrupt); !os.IsNotExist(err) {
		t.Fatalf("old quarantine files must be pruned: %v", err)
	}
}

// TestRunScan_ReclaimsOwnStaleRunningRecords pins CLI-side recovery: a
// running record abandoned by a crashed process is marked interrupted by
// the next scan of the same identity, even with no daemon present.
func TestRunScan_ReclaimsOwnStaleRunningRecords(t *testing.T) {
	dataDir := t.TempDir()
	projectDir := t.TempDir()
	writeGoProject(t, projectDir)

	store := NewScanStore(dataDir)
	stale := time.Now().Add(-2 * time.Hour)
	if err := store.SaveScan(&ScanRecord{
		ScanID:      NewScanID(),
		IdentityKey: "name:myapp",
		AppName:     "myapp",
		Status:      ScanRunning,
		StartedAt:   stale,
	}); err != nil {
		t.Fatal(err)
	}

	server := fakeOSV(t, EcosystemGo, "example.com/vuln", "GO-2024-2001",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "2.0.0")}})
	defer server.Close()

	opts := ScanOptions{
		AppName:     "myapp",
		IdentityKey: "name:myapp",
		Dir:         projectDir,
		Trigger:     TriggerManual,
		DataDir:     dataDir,
		OSVBaseURL:  server.URL,
		GoResolver:  fakeResolver{modules: []ResolvedModule{{Path: "example.com/vuln", Version: "v1.0.0"}}},
	}
	if _, err := RunScan(context.Background(), opts); err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	records, _ := store.ListScans("name:myapp")
	if len(records) != 2 {
		t.Fatalf("two records expected: %d", len(records))
	}
	interrupted := false
	for _, record := range records {
		if record.Status == ScanInterrupted {
			interrupted = true
		}
	}
	if !interrupted {
		t.Fatalf("the abandoned record must be interrupted: %+v", records)
	}
}
