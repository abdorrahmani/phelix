package deploy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/buildreport"
)

// helper: a valid successful build report for tests.
func testReport(sizeMB int64, durMS int64, cache buildreport.CacheStatus) *buildreport.Report {
	return &buildreport.Report{
		Language:        "go",
		Compiler:        "go",
		CompilerVersion: "1.27",
		StartedAt:       time.Now().Add(-time.Duration(durMS) * time.Millisecond),
		EndedAt:         time.Now(),
		DurationMS:      durMS,
		Cache:           buildreport.CacheInfo{Status: cache, Source: buildreport.CacheSourceGoBuild},
		Artifact: buildreport.ArtifactInfo{
			Type:      buildreport.ArtifactBinary,
			SizeBytes: sizeMB * 1024 * 1024,
			Platform:  "linux/amd64",
		},
	}
}

func TestRecordFreshBuild_ReportRoundTrip(t *testing.T) {
	// Simulates a process restart: record → reload versions.json from disk →
	// the build report metadata must survive byte-for-byte semantics.
	resetHome(t)
	app := "report-roundtrip"
	log := &fakeLogger{}

	tmpBin := filepath.Join(t.TempDir(), "myapp")
	if err := os.WriteFile(tmpBin, []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	report := testReport(14, 31200, buildreport.CacheCold)
	rec, err := RecordFreshBuild(app, "1", tmpBin, "8f31c2a", "tag-x", report, DefaultRetention{Max: 5}, log)
	if err != nil {
		t.Fatalf("RecordFreshBuild: %v", err)
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("LoadVersions: %v", err)
	}
	if len(vf.Versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(vf.Versions))
	}
	got := vf.Versions[0].BuildReport
	if got == nil {
		t.Fatal("build report metadata missing after reload")
	}
	if got.Language != "go" || got.CompilerVersion != "1.27" {
		t.Fatalf("unexpected language/toolchain: %+v", got)
	}
	if got.DurationMS != 31200 {
		t.Fatalf("duration = %d, want 31200", got.DurationMS)
	}
	if got.Cache.Status != buildreport.CacheCold {
		t.Fatalf("cache status = %q", got.Cache.Status)
	}
	// Size must be reconciled with the actually stored binary.
	if got.Artifact.SizeBytes != int64(len("fake-binary")) {
		t.Fatalf("artifact size should match stored binary (%d), got %d",
			len("fake-binary"), got.Artifact.SizeBytes)
	}
	// Two-phase invariant still intact with reports present.
	if vf.Versions[0].IsCurrent {
		t.Fatal("version must not be current until promote")
	}
	if err := PromoteVersion(app, rec.Version, "classic"); err != nil {
		t.Fatalf("PromoteVersion: %v", err)
	}
	vf, _ = LoadVersions(app)
	if !vf.Versions[0].IsCurrent {
		t.Fatal("promote failed")
	}
}

func TestLoadVersions_OldMetadataWithoutReport(t *testing.T) {
	resetHome(t)
	app := "legacy-versions"
	// Hand-written legacy versions.json WITHOUT build_report — must load fine.
	legacy := `{"versions":[
		{"version":1,"built_at":"2025-01-01T00:00:00Z","size_bytes":1024,"is_current":false},
		{"version":2,"built_at":"2025-01-02T00:00:00Z","size_bytes":2048,"is_current":true,
		 "tag":"old-tag","git_commit":"deadbeef"}
	]}`
	dir, err := appDataDir(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "versions.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("legacy versions.json must load: %v", err)
	}
	if len(vf.Versions) != 2 || vf.Versions[0].BuildReport != nil {
		t.Fatalf("legacy metadata malformed: %+v", vf)
	}
	// Rollback resolution, listing and pruning keep working on legacy data.
	if _, err := PreviousVersion(app); err != nil {
		t.Fatalf("PreviousVersion on legacy data: %v", err)
	}
	if _, err := ListVersionsForDisplay(app, DefaultRetention{Max: 5}); err != nil {
		t.Fatalf("ListVersionsForDisplay on legacy data: %v", err)
	}
	if err := PruneVersions(app, DefaultRetention{Max: 5}, &fakeLogger{}); err != nil {
		t.Fatalf("PruneVersions on legacy data: %v", err)
	}
}

func TestLoadVersions_MalformedOptionalReportSafe(t *testing.T) {
	resetHome(t)
	app := "malformed-report"
	// build_report payload is garbage (string where object expected). The
	// OPTIONAL field must degrade to nil without failing the whole load.
	bad := `{"versions":[
		{"version":1,"built_at":"2025-01-01T00:00:00Z","size_bytes":1024,"is_current":false,
		 "build_report":"not-an-object"}
	]}`
	dir, err := appDataDir(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "versions.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatalf("malformed optional build_report must not break loading: %v", err)
	}
	if len(vf.Versions) != 1 || vf.Versions[0].BuildReport != nil {
		t.Fatalf("expected report degraded to nil, got %+v", vf.Versions)
	}
	// Core metadata intact; rollback machinery unaffected.
	if _, err := ListVersionsForDisplay(app, DefaultRetention{Max: 5}); err != nil {
		t.Fatalf("listing broken by malformed optional field: %v", err)
	}
	if err := PruneVersions(app, DefaultRetention{Max: 5}, &fakeLogger{}); err != nil {
		t.Fatalf("pruning broken by malformed optional field: %v", err)
	}

	// Malformed CORE fields still surface a proper structured error.
	core := `{"versions":[{"version":"not-a-number"}]}`
	if err := os.WriteFile(filepath.Join(dir, "versions.json"), []byte(core), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVersions(app); err == nil {
		t.Fatal("malformed core metadata must still error")
	}
}

func TestBuildReportHistory_ExcludesCurrentAndOrdersNewestFirst(t *testing.T) {
	resetHome(t)
	app := "history-order"
	log := &fakeLogger{}
	tmpBin := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(tmpBin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		var rep *buildreport.Report
		if i > 0 {
			rep = testReport(int64(10+i), int64(2000+i), buildreport.CacheHit)
		}
		if _, err := RecordFreshBuild(app, "1", tmpBin, "", "", rep, DefaultRetention{Max: 5}, log); err != nil {
			t.Fatalf("record v%d: %v", i+1, err)
		}
	}
	// v3 recorded last with a report; v1 was recorded without one.
	hist, err := BuildReportHistory(app, 3)
	if err != nil {
		t.Fatalf("BuildReportHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("expected 2 previous entries (v2, v1), got %d", len(hist))
	}
	if hist[0].Version != 2 || hist[1].Version != 1 {
		t.Fatalf("newest-first order violated: %d, %d", hist[0].Version, hist[1].Version)
	}
	if hist[0].Report == nil || hist[0].Report.DurationMS != 2001 {
		t.Fatalf("v2 report wrong: %+v", hist[0].Report)
	}
	if hist[1].Report != nil {
		t.Fatalf("legacy v1 must expose nil report for analysis, got %+v", hist[1].Report)
	}
	// Excluding a version must remove exactly that one.
	hist2, _ := BuildReportHistory(app, 2)
	if len(hist2) != 2 {
		t.Fatalf("expected v3 and v1 after excluding v2, got %d entries", len(hist2))
	}
}

func TestRecordMatrixBuild_PersistsPerComboReports(t *testing.T) {
	resetHome(t)
	app := "matrix-reports"

	artifacts := []MatrixArtifact{
		{Platform: "linux/amd64", Version: "1.27", Status: "success", SizeBytes: 100,
			Report: &buildreport.Report{Language: "go", CompilerVersion: "1.27",
				DurationMS: 21000, Cache: buildreport.CacheInfo{Status: buildreport.CacheCold},
				Artifact: buildreport.ArtifactInfo{Type: buildreport.ArtifactBinary, SizeBytes: 100, Platform: "linux/amd64"}}},
		{Platform: "linux/arm64", Version: "1.27", Status: "success", SizeBytes: 110,
			Report: &buildreport.Report{Language: "go", CompilerVersion: "1.27",
				DurationMS: 23100, Cache: buildreport.CacheInfo{Status: buildreport.CacheHit},
				Artifact: buildreport.ArtifactInfo{Type: buildreport.ArtifactBinary, SizeBytes: 110, Platform: "linux/arm64"}}},
	}
	if _, err := RecordMatrixBuild(app, "", "c0ffee", artifacts, "", DefaultRetention{Max: 5}, &fakeLogger{}); err != nil {
		t.Fatalf("RecordMatrixBuild: %v", err)
	}

	// Reload from disk (restart simulation).
	hist, err := BuildReportHistory(app, 999) // exclude nothing real
	if err != nil {
		t.Fatalf("BuildReportHistory: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("expected one entry per successful combo, got %d", len(hist))
	}
	platforms := map[string]bool{}
	for _, h := range hist {
		if h.Report == nil {
			t.Fatal("matrix combo entry missing report")
		}
		platforms[h.Report.Artifact.Platform] = true
	}
	if !platforms["linux/amd64"] || !platforms["linux/arm64"] {
		t.Fatalf("per-combo platform metadata lost: %v", platforms)
	}
}

func TestPruneVersions_WithReportsRetained(t *testing.T) {
	// Retention must keep pruning normally when versions carry reports; the
	// current version is never pruned.
	resetHome(t)
	app := "prune-with-reports"
	log := &fakeLogger{}
	tmpBin := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(tmpBin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := RecordFreshBuild(app, "1", tmpBin, "", "", testReport(10, int64(1000+i), buildreport.CacheHit), DefaultRetention{Max: 3}, log); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(vf.Versions) > 3 {
		t.Fatalf("retention exceeded: %d versions", len(vf.Versions))
	}
	for _, v := range vf.Versions {
		if v.IsCurrent && v.BuildReport == nil {
			t.Fatal("current version lost its report")
		}
	}
	// History only reflects retained versions.
	hist, _ := BuildReportHistory(app, 0)
	if len(hist) != len(vf.Versions) {
		t.Fatalf("history %d out of sync with versions %d", len(hist), len(vf.Versions))
	}
}
