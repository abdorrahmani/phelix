package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanStore_SaveLoadRoundTrip(t *testing.T) {
	store := NewScanStore(t.TempDir())
	finished := time.Now()
	record := &ScanRecord{
		ScanID:      NewScanID(),
		AppName:     "myapp",
		IdentityKey: "11111111-1111-4111-8111-111111111111",
		Trigger:     TriggerManual,
		Status:      ScanCompleted,
		StartedAt:   finished.Add(-time.Minute),
		FinishedAt:  &finished,
		Freshness:   FreshnessFresh,
		Counts:      ScanCounts{Affected: 1, BySeverity: map[string]int{SeverityHigh: 1}},
	}
	if err := store.SaveScan(record); err != nil {
		t.Fatalf("SaveScan: %v", err)
	}
	if record.SchemaVersion != scanSchemaVersion {
		t.Fatal("SaveScan must stamp the schema version")
	}
	loaded, err := store.LoadScan(record.ScanID)
	if err != nil {
		t.Fatalf("LoadScan: %v", err)
	}
	if loaded.AppName != "myapp" || loaded.Status != ScanCompleted {
		t.Fatalf("round trip lost data: %+v", loaded)
	}
	info, err := os.Stat(store.scanPath(record.ScanID))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("scan records must be private: %v", perm)
	}
}

func TestScanStore_RejectsBadIDs(t *testing.T) {
	store := NewScanStore(t.TempDir())
	if err := store.SaveScan(&ScanRecord{ScanID: "nope"}); err == nil {
		t.Fatal("foreign scan ids must be rejected")
	}
	if _, err := store.LoadScan("../../etc/passwd"); err == nil {
		t.Fatal("path-like ids must be rejected")
	}
}

func TestScanStore_CorruptRecordQuarantined(t *testing.T) {
	store := NewScanStore(t.TempDir())
	if err := os.MkdirAll(store.scansDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := store.scanPath(NewScanID())
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".json")
	if _, err := store.LoadScan(id); err == nil {
		t.Fatal("corrupt records must fail closed")
	}
	matches, _ := filepath.Glob(path + ".corrupt.*")
	if len(matches) != 1 {
		t.Fatalf("corrupt record must be quarantined: %v", matches)
	}
}

func TestScanStore_ListScansAndRetain(t *testing.T) {
	store := NewScanStore(t.TempDir())
	identity := "name:myapp"
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 4; i++ {
		started := base.Add(time.Duration(i) * time.Minute)
		status := ScanCompleted
		if i == 3 {
			status = ScanRunning
		}
		if err := store.SaveScan(&ScanRecord{
			ScanID:      NewScanID(),
			IdentityKey: identity,
			Status:      status,
			StartedAt:   started,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveScan(&ScanRecord{
		ScanID:      NewScanID(),
		IdentityKey: "name:other",
		Status:      ScanCompleted,
		StartedAt:   base,
	}); err != nil {
		t.Fatal(err)
	}

	records, err := store.ListScans(identity)
	if err != nil {
		t.Fatalf("ListScans: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("identity filter leaked: %d records", len(records))
	}
	for i := 1; i < len(records); i++ {
		if records[i-1].StartedAt.Before(records[i].StartedAt) {
			t.Fatal("ListScans must return newest first")
		}
	}

	if err := store.Retain(identity, 2); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	records, _ = store.ListScans(identity)
	if len(records) != 2 {
		t.Fatalf("retention must keep the newest 2 records: %d", len(records))
	}
	for _, record := range records {
		if record.Status == ScanRunning {
			continue
		}
		if record.StartedAt.Before(base.Add(2 * time.Minute)) {
			t.Fatalf("retention removed the wrong record: %s", record.ScanID)
		}
	}
	hasRunning := false
	for _, record := range records {
		if record.Status == ScanRunning {
			hasRunning = true
		}
	}
	if !hasRunning {
		t.Fatal("a running record must never be removed by retention")
	}
}

func TestScanStore_LatestIndex(t *testing.T) {
	store := NewScanStore(t.TempDir())
	identity := "name:myapp"
	finished := time.Now()
	next := finished.Add(24 * time.Hour)

	if err := store.MarkLatestSuccess(identity, "scn_0000000000000001", finished); err != nil {
		t.Fatalf("MarkLatestSuccess: %v", err)
	}
	if err := store.ScheduleNext(identity, next); err != nil {
		t.Fatalf("ScheduleNext: %v", err)
	}
	entry, err := store.Latest(identity)
	if err != nil || entry == nil {
		t.Fatalf("Latest: %v", err)
	}
	if entry.ScanID != "scn_0000000000000001" || entry.NextScanAt == nil {
		t.Fatalf("latest entry wrong: %+v", entry)
	}

	if err := store.MarkLatestSuccess(identity, "scn_0000000000000002", finished); err != nil {
		t.Fatal(err)
	}
	entry, _ = store.Latest(identity)
	if entry.ScanID != "scn_0000000000000002" || entry.NextScanAt == nil {
		t.Fatalf("a success must preserve the scheduled time: %+v", entry)
	}
}

func TestScanStore_LatestFallsBackToRecords(t *testing.T) {
	store := NewScanStore(t.TempDir())
	identity := "name:myapp"
	finished := time.Now()
	if err := store.SaveScan(&ScanRecord{
		ScanID:      NewScanID(),
		IdentityKey: identity,
		Status:      ScanCompleted,
		StartedAt:   finished.Add(-time.Minute),
		FinishedAt:  &finished,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.latestPath()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	entry, err := store.Latest(identity)
	if err != nil || entry == nil || entry.ScanID == "" {
		t.Fatalf("a lost index must be rebuilt from scan records: %v", err)
	}
}

func TestScanStore_ScanLockContention(t *testing.T) {
	store := NewScanStore(t.TempDir())
	identity := "name:myapp"

	first, err := store.AcquireScanLock(identity)
	if err != nil {
		t.Fatalf("AcquireScanLock: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		second, err := store.AcquireScanLock(identity)
		if err != nil {
			close(acquired)
			return
		}
		second.Release()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second lock must block while the first is held")
	case <-time.After(150 * time.Millisecond):
	}
	first.Release()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock must be granted after release")
	}
}

func TestScanStore_DifferentIdentitiesDoNotContend(t *testing.T) {
	store := NewScanStore(t.TempDir())
	first, err := store.AcquireScanLock("name:a")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := store.AcquireScanLock("name:b")
	if err != nil {
		t.Fatalf("different identities must not block each other: %v", err)
	}
	second.Release()
}

func TestLockFileName(t *testing.T) {
	name := lockFileName("name:my app/odd:path")
	if strings.ContainsAny(name, ":/ ") {
		t.Fatalf("lock file names must be safe: %q", name)
	}
	if name != "name-my-app-odd-path.lock" {
		t.Fatalf("unexpected lock name %q", name)
	}
}

func TestFingerprintFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prints := FingerprintFiles(dir, []string{"go.mod", "go.sum"})
	if len(prints) != 1 {
		t.Fatalf("missing files must be skipped: %v", prints)
	}
	if len(prints["go.mod"]) != 64 {
		t.Fatalf("fingerprints must be sha256 hex: %q", prints["go.mod"])
	}
}
