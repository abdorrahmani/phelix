package matrix

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// manifestFixture builds a finished run with per-combination outcomes. The
// artifacts point at real files so size and checksum propagation can be
// asserted.
func manifestFixture(t *testing.T, outcomes ...string) *Run {
	t.Helper()
	dir := t.TempDir()
	run := NewRun("mx_20260909_8f31", "app", dir, &Profile{
		Lang: builder.Go, Versions: []string{"1.26", "1.27"}, Platforms: []string{"linux/amd64", "linux/arm64"}, Concurrency: 2,
	}, time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC))

	combs := []Combination{
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "amd64", Platform: "linux/amd64"},
		{Lang: builder.Go, Version: "1.26", OS: "linux", Arch: "arm64", Platform: "linux/arm64"},
	}
	run.InitCombinations(combs)
	for i, outcome := range outcomes {
		switch outcome {
		case "success":
			bin := filepath.Join(dir, combs[i%len(combs)].ID()+".bin")
			if err := os.WriteFile(bin, []byte("artifact "+combs[i%len(combs)].ID()), 0o755); err != nil {
				t.Fatal(err)
			}
			sum, err := SHA256File(bin)
			if err != nil {
				t.Fatal(err)
			}
			run.RecordResult(Result{
				Combination: combs[i%len(combs)],
				Status:      "success",
				Artifact:    bin,
				SHA256:      sum,
				Attempts:    []Attempt{{Number: 1, Status: "success"}},
			})
		case "failed":
			run.RecordResult(Result{
				Combination: combs[i%len(combs)],
				Status:      "failed",
				Error:       phelixerr.New(phelixerr.CodeBuildFailed, "boom"),
			})
		}
	}
	run.Finalize(time.Date(2026, 9, 9, 10, 5, 0, 0, time.UTC))
	return run
}

func TestBuildReleaseManifest_Complete(t *testing.T) {
	run := manifestFixture(t, "success", "success", "success", "success")
	m, err := BuildReleaseManifest(run, 13, "v1.4.0", time.Date(2026, 9, 9, 10, 6, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != ReleaseStatusComplete {
		t.Fatalf("status = %s, want complete", m.Status)
	}
	if m.MatrixRunID != run.ID || m.AppName != "app" || m.Version != 13 || m.Tag != "v1.4.0" {
		t.Fatalf("identity: %+v", m)
	}
	if m.TotalCombinations != 4 || len(m.Artifacts) != 4 {
		t.Fatalf("counts: total=%d artifacts=%d", m.TotalCombinations, len(m.Artifacts))
	}
	// Deterministic ordering: sorted by combination ID, not completion order.
	wantOrder := []string{"go1.26-linux-amd64", "go1.26-linux-arm64", "go1.27-linux-amd64", "go1.27-linux-arm64"}
	for i, want := range wantOrder {
		if m.Artifacts[i].CombinationID != want {
			t.Fatalf("artifact[%d] = %s, want %s (ordering not deterministic)", i, m.Artifacts[i].CombinationID, want)
		}
	}
	// Checksums propagate from the run into the manifest.
	for _, a := range m.Artifacts {
		if len(a.SHA256) != SHA256HexLen {
			t.Fatalf("artifact %s has no checksum: %+v", a.CombinationID, a)
		}
		if a.SizeBytes == 0 || a.Identity != string(run.ID)+"/"+a.CombinationID {
			t.Fatalf("artifact identity/size incomplete: %+v", a)
		}
	}
}

func TestBuildReleaseManifest_PartialIsExplicit(t *testing.T) {
	run := manifestFixture(t, "success", "success", "success", "failed")
	if run.Status != RunStatusPartial {
		t.Fatalf("fixture status = %s", run.Status)
	}
	m, err := BuildReleaseManifest(run, 13, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != ReleaseStatusPartial {
		t.Fatalf("partial run must produce a partial manifest, got %s", m.Status)
	}
	if len(m.Artifacts) != 3 || m.TotalCombinations != 4 {
		t.Fatalf("partial manifest must list only successful artifacts: %d of %d", len(m.Artifacts), m.TotalCombinations)
	}
}

func TestBuildReleaseManifest_RejectsNonReleases(t *testing.T) {
	// Interrupted: incomplete combinations mean the run is not a release.
	interrupted := manifestFixture(t, "success", "success", "pending", "pending")
	if interrupted.Status != RunStatusInterrupted {
		t.Fatalf("fixture status = %s, want interrupted", interrupted.Status)
	}
	if _, err := BuildReleaseManifest(interrupted, 1, "", time.Now()); err == nil {
		t.Fatal("interrupted run must not gain a release manifest")
	}

	// Failed: nothing succeeded, no artifact set to describe.
	failed := manifestFixture(t, "failed", "failed", "failed", "failed")
	if failed.Status != RunStatusFailed {
		t.Fatalf("fixture status = %s, want failed", failed.Status)
	}
	if _, err := BuildReleaseManifest(failed, 1, "", time.Now()); err == nil {
		t.Fatal("failed run must not gain a release manifest")
	}

	// Still running.
	running := NewRun("mx_20260909_0001", "app", "/tmp", nil, time.Now())
	if _, err := BuildReleaseManifest(running, 1, "", time.Now()); err == nil {
		t.Fatal("running run must not gain a release manifest")
	}

	// Not recorded under an application version.
	done := manifestFixture(t, "success")
	if _, err := BuildReleaseManifest(done, 0, "", time.Now()); err == nil {
		t.Fatal("version <= 0 must be rejected")
	}
	if _, err := BuildReleaseManifest(nil, 1, "", time.Now()); err == nil {
		t.Fatal("nil run must be rejected")
	}
}

func TestBuildReleaseManifest_SkipsPhantomAndDuplicateArtifacts(t *testing.T) {
	// Two successes, two failures: a terminal partial run that qualifies for
	// a manifest.
	run := manifestFixture(t, "success", "failed", "success", "failed")
	if run.Status != RunStatusPartial {
		t.Fatalf("fixture status = %s, want partial", run.Status)
	}

	// Simulate the two data bugs the manifest must tolerate: a success entry
	// without an artifact (nothing to reference or verify → no entry), and a
	// duplicated combination entry (dedupe by ID).
	var dup RunCombination
	for i := range run.Combinations {
		rc := &run.Combinations[i]
		if rc.Status != "success" {
			continue
		}
		if dup.ID == "" && rc.Artifact != "" {
			dup = *rc // keep one real success to duplicate
			continue
		}
		rc.Artifact = "" // phantom success: artifact information lost
		rc.SHA256 = ""
	}
	run.Combinations = append(run.Combinations, dup)

	m, err := BuildReleaseManifest(run, 5, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Artifacts) != 1 {
		t.Fatalf("phantom and duplicate entries leaked into the manifest: %+v", m.Artifacts)
	}
	if m.Artifacts[0].CombinationID == "" || m.Artifacts[0].Artifact == "" || m.Artifacts[0].SHA256 == "" {
		t.Fatalf("the one real artifact must be fully described: %+v", m.Artifacts[0])
	}
	seen := map[string]int{}
	for _, a := range m.Artifacts {
		seen[a.CombinationID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("combination %s appears %d times in the manifest", id, n)
		}
	}
}

func TestBuildReleaseManifest_DeterministicBytes(t *testing.T) {
	run := manifestFixture(t, "success", "success", "success", "failed")
	createdAt := time.Date(2026, 9, 9, 10, 6, 0, 0, time.UTC)

	first, err := BuildReleaseManifest(run, 7, "v2", createdAt)
	if err != nil {
		t.Fatal(err)
	}
	// A second object with the same logical content (a fresh clone of the
	// run) must serialize identically — no map iteration order, no completion
	// order, no timestamps inside the artifact list.
	twin := run.Clone()
	second, err := BuildReleaseManifest(twin, 7, "v2", createdAt)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.MarshalIndent(first, "", "  ")
	b, _ := json.MarshalIndent(second, "", "  ")
	if string(a) != string(b) {
		t.Fatalf("manifest generation is not deterministic:\n%s\n---\n%s", a, b)
	}
}

func TestManifestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	run := manifestFixture(t, "success", "success", "failed", "failed")
	m, err := BuildReleaseManifest(run, 9, "v1.4.0", time.Date(2026, 9, 9, 10, 6, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveManifest(m); err != nil {
		t.Fatal(err)
	}

	path, err := ManifestPath(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("manifest not written next to the run record: %v", err)
	}

	loaded, err := LoadManifest(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MatrixRunID != m.MatrixRunID || loaded.Version != m.Version || loaded.Status != m.Status {
		t.Fatalf("roundtrip mismatch: %+v", loaded)
	}
	if len(loaded.Artifacts) != len(m.Artifacts) {
		t.Fatalf("artifact count changed through persistence: %d vs %d", len(loaded.Artifacts), len(m.Artifacts))
	}
}

func TestManifestLoadNotFoundAndMalformed(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	_, err := LoadManifest("mx_20260909_8f31")
	if phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Fatalf("missing manifest must be a clean not-found, got %v", err)
	}

	m := &ReleaseManifest{
		SchemaVersion: manifestSchemaVersion, AppName: "app", Version: 1,
		MatrixRunID: "mx_20260909_8f31", CreatedAt: time.Now(),
		Status: ReleaseStatusComplete, Artifacts: []ManifestArtifact{{CombinationID: "go1.26-linux-amd64"}},
	}
	if err := SaveManifest(m); err != nil {
		t.Fatal(err)
	}
	// A manifest claiming a different run ID than the file it lives in is
	// malformed.
	path, _ := ManifestPath("mx_20260909_8f31")
	m.MatrixRunID = "mx_20260909_beef"
	data, _ := json.Marshal(m)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest("mx_20260909_8f31"); err == nil {
		t.Fatal("manifest claiming another run must be rejected")
	}
}

func TestSaveManifest_RejectsInvalidManifests(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	base := ReleaseManifest{
		SchemaVersion: manifestSchemaVersion, AppName: "app", Version: 1,
		MatrixRunID: "mx_20260909_8f31", CreatedAt: time.Now(),
		Status: ReleaseStatusComplete,
	}
	if err := SaveManifest(&base); err != nil {
		t.Fatal(err)
	}

	noVersion := base
	noVersion.Version = 0
	if err := SaveManifest(&noVersion); err == nil {
		t.Fatal("manifest without an application version must be rejected")
	}

	badStatus := base
	badStatus.Status = "complete-ish"
	if err := SaveManifest(&badStatus); err == nil {
		t.Fatal("manifest with an unknown status must be rejected")
	}
	if err := SaveManifest(nil); err == nil {
		t.Fatal("nil manifest must be rejected")
	}
}

func TestListRunsSkipsManifestFiles(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	run := manifestFixture(t, "success", "success", "failed", "failed")
	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}
	m, err := BuildReleaseManifest(run, 3, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveManifest(m); err != nil {
		t.Fatal(err)
	}

	runs, skipped, err := ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || skipped != 0 {
		t.Fatalf("manifest file leaked into the run list: runs=%d skipped=%d", len(runs), skipped)
	}
	if runs[0].ID != run.ID {
		t.Fatalf("wrong run listed: %s", runs[0].ID)
	}
}
