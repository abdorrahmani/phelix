package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdvisoryCache_RoundTrip(t *testing.T) {
	cache := NewAdvisoryCache(t.TempDir())
	adv := &OSVAdvisory{ID: "GO-2024-1001", Summary: "example"}
	if err := cache.Put(adv, time.Now()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, stale, err := cache.Get("GO-2024-1001")
	if err != nil || got == nil || stale {
		t.Fatalf("fresh entry must be returned non-stale: err=%v stale=%v", err, stale)
	}
	if got.Summary != "example" {
		t.Fatalf("round trip lost content: %+v", got)
	}
}

func TestAdvisoryCache_Expiry(t *testing.T) {
	dir := t.TempDir()
	cache := NewAdvisoryCache(dir)
	adv := &OSVAdvisory{ID: "GO-2024-1001"}
	if err := cache.Put(adv, time.Now()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	cache.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	_, stale, err := cache.Get("GO-2024-1001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !stale {
		t.Fatal("an entry past the TTL must report stale")
	}
}

func TestAdvisoryCache_CorruptEntriesAreQuarantined(t *testing.T) {
	dir := t.TempDir()
	cache := NewAdvisoryCache(dir)
	path := filepath.Join(dir, "GO-2024-1001.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	adv, stale, err := cache.Get("GO-2024-1001")
	if err != nil || adv != nil || stale {
		t.Fatalf("corrupt entry must be treated as absent: err=%v", err)
	}
	remaining, _ := filepath.Glob(filepath.Join(dir, "GO-2024-1001.json*"))
	if len(remaining) != 1 || !strings.HasSuffix(remaining[0], ".corrupt.") && !strings.Contains(remaining[0], ".corrupt.") {
		t.Fatalf("corrupt entry must be quarantined, found %v", remaining)
	}
}

func TestAdvisoryCache_ForeignOrMismatchedEntriesAreQuarantined(t *testing.T) {
	dir := t.TempDir()
	cache := NewAdvisoryCache(dir)
	foreign := `{"schema_version": 99, "id": "GO-2024-1001", "fetched_at": "2024-01-01T00:00:00Z", "advisory": {"id": "GO-2024-1001"}}`
	if err := os.WriteFile(filepath.Join(dir, "GO-2024-1001.json"), []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	mismatch := `{"schema_version": 1, "id": "GO-2024-1001", "fetched_at": "2024-01-01T00:00:00Z", "advisory": {"id": "GO-2024-9999"}}`
	if err := os.WriteFile(filepath.Join(dir, "GO-2024-1002.json"), []byte(mismatch), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"GO-2024-1001", "GO-2024-1002"} {
		adv, _, err := cache.Get(id)
		if err != nil || adv != nil {
			t.Fatalf("entry %s must be rejected: err=%v", id, err)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.corrupt.*"))
	if len(matches) != 2 {
		t.Fatalf("both entries must be quarantined: %v", matches)
	}
}

func TestAdvisoryCache_Prune(t *testing.T) {
	dir := t.TempDir()
	cache := NewAdvisoryCache(dir)
	old := time.Now().Add(-10 * 24 * time.Hour)
	if err := cache.Put(&OSVAdvisory{ID: "GO-2024-1001"}, old); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(&OSVAdvisory{ID: "GO-2024-1002"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	cache.Prune(AdvisoryCachePruneAge)
	if _, _, err := cache.Get("GO-2024-1001"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "GO-2024-1001.json")); !os.IsNotExist(err) {
		t.Fatalf("pruned entry must be gone: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "GO-2024-1002.json")); err != nil {
		t.Fatalf("fresh entry must survive pruning: err=%v", err)
	}
}

func TestAdvisoryCache_RejectsInvalidID(t *testing.T) {
	cache := NewAdvisoryCache(t.TempDir())
	if _, _, err := cache.Get("../escape"); err == nil {
		t.Fatal("invalid ids must be rejected")
	}
	if err := cache.Put(&OSVAdvisory{ID: ""}, time.Now()); err == nil {
		t.Fatal("advisories without an id must not be cached")
	}
}
