package security

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOSV serves a batch query endpoint and advisory detail endpoint for
// one vulnerable package, so RunScan exercises the full retrieval path.
func fakeOSV(t *testing.T, eco Ecosystem, pkg, advisoryID string, ranges []OSVRange) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/querybatch":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[{"vulns":[{"id":"` + advisoryID + `"}]}]}`))
		case strings.HasPrefix(r.URL.Path, "/v1/vulns/"):
			adv := mkAdv(advisoryID, eco, pkg, ranges, nil, "HIGH")
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(adv)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
}

func writeGoProject(t *testing.T, dir string) {
	t.Helper()
	content := "module example.com/app\n\ngo 1.24\n\nrequire example.com/vuln v1.2.0\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunScan_CompletesAndPersists(t *testing.T) {
	dataDir := t.TempDir()
	projectDir := t.TempDir()
	writeGoProject(t, projectDir)

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
	}
	result, err := RunScan(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}
	record := result.Record
	if record.Status != ScanCompleted || record.FinishedAt == nil {
		t.Fatalf("scan must complete: %+v", record)
	}
	if len(result.Open) != 1 || result.Open[0].State != StatePotentiallyAffected {
		t.Fatalf("expected one potentially affected finding: %+v", result.Open)
	}
	if result.Open[0].Severity != SeverityHigh {
		t.Fatalf("severity must flow from the advisory: %+v", result.Open[0])
	}
	if record.Counts.Affected != 0 || record.Counts.PotentiallyAffected != 1 || record.Counts.NotAffected != 0 {
		t.Fatalf("counts wrong: %+v", record.Counts)
	}
	if record.Freshness != FreshnessFresh {
		t.Fatalf("online scan must be fresh: %s", record.Freshness)
	}
	if record.SourceFiles["go.mod"] == "" {
		t.Fatalf("dependency files must be fingerprinted: %v", record.SourceFiles)
	}
	if record.Inventory == nil || len(record.Inventory.Modules) != 1 {
		t.Fatalf("inventory must be stored: %+v", record.Inventory)
	}

	store := NewScanStore(dataDir)
	loaded, err := store.LoadScan(record.ScanID)
	if err != nil || loaded.Status != ScanCompleted {
		t.Fatalf("record must be persisted: err=%v", err)
	}
	entry, err := store.Latest("name:myapp")
	if err != nil || entry == nil || entry.ScanID != record.ScanID {
		t.Fatalf("latest index must point at the completed scan: %+v", entry)
	}
}

func TestRunScan_OfflineFallsBackToCache(t *testing.T) {
	dataDir := t.TempDir()
	projectDir := t.TempDir()
	writeGoProject(t, projectDir)

	server := fakeOSV(t, EcosystemGo, "example.com/vuln", "GO-2024-2001",
		[]OSVRange{{Type: "ECOSYSTEM", Events: events("introduced", "0", "fixed", "2.0.0")}})

	opts := ScanOptions{
		AppName:     "myapp",
		IdentityKey: "name:myapp",
		Dir:         projectDir,
		Trigger:     TriggerManual,
		DataDir:     dataDir,
		OSVBaseURL:  server.URL,
	}
	if _, err := RunScan(context.Background(), opts); err != nil {
		t.Fatalf("online scan: %v", err)
	}
	server.Close()

	result, err := RunScan(context.Background(), opts)
	if err != nil {
		t.Fatalf("offline scan must succeed from cache: %v", err)
	}
	if result.Record.Freshness != FreshnessStale {
		t.Fatalf("offline scan must report stale data: %s", result.Record.Freshness)
	}
	joined := ""
	for _, warning := range result.Record.Warnings {
		joined += warning + "\n"
	}
	if !strings.Contains(joined, "cached advisory data") {
		t.Fatalf("offline fallback must be prominent: %v", result.Record.Warnings)
	}
	if len(result.Open) != 1 || result.Open[0].State != StatePotentiallyAffected {
		t.Fatalf("cached advisories must still match: %+v", result.Open)
	}
}

func TestRunScan_NoNetworkNoCacheFails(t *testing.T) {
	dataDir := t.TempDir()
	projectDir := t.TempDir()
	writeGoProject(t, projectDir)

	opts := ScanOptions{
		AppName:     "myapp",
		IdentityKey: "name:myapp",
		Dir:         projectDir,
		Trigger:     TriggerManual,
		DataDir:     dataDir,
		OSVBaseURL:  "http://127.0.0.1:1",
	}
	if _, err := RunScan(context.Background(), opts); err == nil {
		t.Fatal("a scan with no source and no cache must fail")
	}
}

func TestRunScan_RunningRecordSurvivesAsInterrupted(t *testing.T) {
	store := NewScanStore(t.TempDir())
	identity := "name:myapp"
	started := time.Now().Add(-2 * time.Hour)
	if err := store.SaveScan(&ScanRecord{
		ScanID:      NewScanID(),
		IdentityKey: identity,
		Status:      ScanRunning,
		StartedAt:   started,
	}); err != nil {
		t.Fatal(err)
	}
	records, _ := store.ListScans(identity)
	if len(records) != 1 || records[0].Status != ScanRunning {
		t.Fatalf("running record must be listable: %+v", records)
	}
}

func TestSaveFailedScan_PreservesLatest(t *testing.T) {
	dataDir := t.TempDir()
	projectDir := t.TempDir()
	writeGoProject(t, projectDir)

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
	}
	result, err := RunScan(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunScan: %v", err)
	}

	failed, err := SaveFailedScan(opts, context.DeadlineExceeded, nil)
	if err != nil || failed == nil {
		t.Fatalf("SaveFailedScan: %v", err)
	}
	if failed.Status != ScanFailed || failed.Outcome == "" {
		t.Fatalf("failed record must carry an outcome: %+v", failed)
	}
	entry, _ := NewScanStore(dataDir).Latest("name:myapp")
	if entry.ScanID != result.Record.ScanID {
		t.Fatalf("a failed scan must not replace the latest success: %+v", entry)
	}
}

func TestSaveFailedScan_WithoutIdentityStoresNothing(t *testing.T) {
	record, err := SaveFailedScan(ScanOptions{IdentityKey: ""}, context.Canceled, nil)
	if err != nil || record != nil {
		t.Fatalf("unattributable failures must not be stored: %v %v", record, err)
	}
}
