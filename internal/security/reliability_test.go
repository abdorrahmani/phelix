package security

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFailedAdvisoryDetailIsPartial(t *testing.T) {
	project := t.TempDir()
	writeGoProject(t, project)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/querybatch" {
			_, _ = w.Write([]byte(`{"results":[{"vulns":[{"id":"GO-2024-2001"}]}]}`))
			return
		}
		http.Error(w, "missing advisory", http.StatusNotFound)
	}))
	defer srv.Close()
	result, err := RunScan(context.Background(), ScanOptions{IdentityKey: "app", Dir: project, DataDir: t.TempDir(), OSVBaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Coverage != CoveragePartial || len(result.Record.Warnings) == 0 {
		t.Fatalf("missing advisory presented as complete: %+v", result.Record)
	}
}

func TestCoverageExcludesUnrelatedCachedPackages(t *testing.T) {
	source := NewPrefetchSource(NewAdvisoryCache(t.TempDir()), nil)
	source.attempted[queryKey(EcosystemGo, "target")] = true
	source.byQuery[queryKey(EcosystemGo, "other")] = []string{"GO-2024-2001"}
	resolved, total := source.Coverage()
	if resolved != 0 || total != 1 {
		t.Fatalf("coverage = %d/%d", resolved, total)
	}
}

func TestSeverityGateExcludesNotAffected(t *testing.T) {
	_, counts, _ := buildCounts(&Inventory{}, []Finding{{State: StateNotAffected, Severity: SeverityCritical}})
	if counts.BySeverity[SeverityCritical] != 0 || counts.NotAffected != 1 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestNextScanTimeUsesCurrentInterval(t *testing.T) {
	store := NewScanStore(t.TempDir())
	finished := time.Now()
	record := &ScanRecord{ScanID: NewScanID(), IdentityKey: "app", Status: ScanCompleted, StartedAt: finished, FinishedAt: &finished}
	if err := store.SaveScan(record); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkLatestSuccess("app", record.ScanID, finished); err != nil {
		t.Fatal(err)
	}
	if err := store.ScheduleNext("app", finished.Add(24*time.Hour), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	next, err := store.NextScanTime("app", time.Hour)
	if err != nil || next == nil || !next.Equal(finished.Add(time.Hour)) {
		t.Fatalf("new schedule = %v: %v", next, err)
	}
}
