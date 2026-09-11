package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the architectural invariant of matrix versioning:
//
//	1 logical application version + N matrix combinations = 1 release + N
//	artifacts — NEVER N application versions.
//
// A matrix build is one logical build of one application version that happens
// to produce multiple platform artifacts. The combinations (go1.27-linux-amd64,
// go1.27-linux-arm64, …) are artifacts of that release; they must not become
// independent versions in versions.json.

func TestRecordMatrixBuild_OneVersionManyArtifacts(t *testing.T) {
	resetHome(t)
	app := "matrix-invariant"

	artifacts := []MatrixArtifact{
		{Platform: "linux/amd64", Version: "1.27", Status: "success", SizeBytes: 100,
			MatrixRunID: "mx_20260910_8f31", SHA256: strings.Repeat("a", 64)},
		{Platform: "linux/arm64", Version: "1.27", Status: "success", SizeBytes: 110,
			MatrixRunID: "mx_20260910_8f31", SHA256: strings.Repeat("b", 64)},
		{Platform: "darwin/arm64", Version: "1.27", Status: "success", SizeBytes: 120,
			MatrixRunID: "mx_20260910_8f31", SHA256: strings.Repeat("c", 64)},
		{Platform: "windows/amd64", Version: "1.27", Status: "success", SizeBytes: 130,
			MatrixRunID: "mx_20260910_8f31", SHA256: strings.Repeat("d", 64)},
	}
	rec, err := RecordMatrixBuild(app, "v1.4.0", "c0ffee", artifacts, "", DefaultRetention{Max: 5}, &fakeLogger{})
	if err != nil {
		t.Fatalf("RecordMatrixBuild: %v", err)
	}

	// Reload from disk (restart simulation) and assert the invariant.
	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(vf.Versions) != 1 {
		t.Fatalf("a 4-combination matrix build must record exactly ONE application version, got %d", len(vf.Versions))
	}
	v := vf.Versions[0]
	if v.Version != rec.Version || v.Tag != "v1.4.0" {
		t.Fatalf("recorded version mismatch: %+v (rec %+v)", v, rec)
	}
	if len(v.MatrixArtifacts) != 4 {
		t.Fatalf("the single version must carry all 4 artifacts, got %d", len(v.MatrixArtifacts))
	}
	// Every artifact stays traceable to its matrix run and carries its
	// checksum — the run → combination → artifact → sha256 chain.
	platforms := map[string]MatrixArtifact{}
	for _, a := range v.MatrixArtifacts {
		if a.MatrixRunID != "mx_20260910_8f31" {
			t.Fatalf("artifact %s lost its matrix run ID: %+v", a.Platform, a)
		}
		if len(a.SHA256) != 64 {
			t.Fatalf("artifact %s lost its checksum: %+v", a.Platform, a)
		}
		platforms[a.Platform] = a
	}
	for _, want := range []string{"linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64"} {
		if _, ok := platforms[want]; !ok {
			t.Fatalf("platform %s missing from the release: %+v", want, v.MatrixArtifacts)
		}
	}
}

func TestMatrixAndFreshBuildsVersionIndependently(t *testing.T) {
	// Normal (non-matrix) builds keep their own version semantics: one
	// binary, one version — and both kinds coexist in one history without
	// the matrix ever exploding into per-combination versions.
	resetHome(t)
	app := "mixed-history"
	log := &fakeLogger{}
	tmpBin := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(tmpBin, []byte("normal binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	fresh, err := RecordFreshBuild(app, "1", tmpBin, "", "normal", nil, DefaultRetention{Max: 5}, log)
	if err != nil {
		t.Fatal(err)
	}
	matrix, err := RecordMatrixBuild(app, "matrix", "", []MatrixArtifact{
		{Platform: "linux/amd64", Version: "1.27", Status: "success", MatrixRunID: "mx_20260910_8f31", SHA256: strings.Repeat("e", 64)},
		{Platform: "linux/arm64", Version: "1.27", Status: "success", MatrixRunID: "mx_20260910_8f31", SHA256: strings.Repeat("f", 64)},
	}, "", DefaultRetention{Max: 5}, log)
	if err != nil {
		t.Fatal(err)
	}

	vf, err := LoadVersions(app)
	if err != nil {
		t.Fatal(err)
	}
	if len(vf.Versions) != 2 {
		t.Fatalf("normal + matrix builds must yield exactly two versions, got %d", len(vf.Versions))
	}
	if vf.Versions[0].Version != fresh.Version || vf.Versions[1].Version != matrix.Version {
		t.Fatalf("version order wrong: %+v", vf.Versions)
	}
	if len(vf.Versions[1].MatrixArtifacts) != 2 {
		t.Fatalf("matrix version artifacts: %+v", vf.Versions[1])
	}
	if len(vf.Versions[0].MatrixArtifacts) != 0 {
		t.Fatalf("normal build must stay single-artifact: %+v", vf.Versions[0])
	}
}
