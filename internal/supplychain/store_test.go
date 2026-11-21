package supplychain

import (
	"context"
	"encoding/json"
	"github.com/abdorrahmani/phelix/internal/security"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixtureReport() *Report {
	return &Report{SchemaVersion: 1, AppID: "app_test", AppName: "example", Version: 1, BuiltAt: time.Unix(1, 0).UTC(), Artifact: Artifact{Filename: "binary", SHA256: Digest([]byte("artifact")), SizeBytes: 8}, Inventory: &security.Inventory{Ecosystem: security.EcosystemGo, Level: security.InventoryResolved}, Policy: DefaultPolicy(), Signature: Signature{Status: "missing"}}
}

func TestStoreOwnershipAndTampering(t *testing.T) {
	s := NewStore(t.TempDir())
	r := fixtureReport()
	d, err := s.PutDocument("spdx-2.3", []byte(`{"value":"<literal>"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.SBOMs = []Document{d}
	if err = s.Save(r); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(r.AppID, reportKey(r)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load("other", reportKey(r)); err == nil {
		t.Fatal("cross app load succeeded")
	}
	for _, v := range []string{"../escape", "/absolute", ".."} {
		if _, err = s.Load(v, reportKey(r)); err == nil {
			t.Fatalf("unsafe identity accepted %q", v)
		}
	}
	path := filepath.Join(s.root(), "documents", d.SHA256+".json")
	if err = os.WriteFile(path, []byte(`{"value":"tampered"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Document(d.SHA256); err == nil {
		t.Fatal("tampered document accepted")
	}
}

func TestPolicyNeverTreatsUnavailableScanAsClean(t *testing.T) {
	r := fixtureReport()
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.RequireSBOM = false
	p.RequireProvenance = false
	p.RequireScan = true
	if _, err := Evaluate(r, p); err == nil {
		t.Fatal("missing scan accepted")
	}
	r.Scan = &security.ScanRecord{Status: security.ScanCompleted, Coverage: security.CoveragePartial, Freshness: security.FreshnessFresh}
	if _, err := Evaluate(r, p); err == nil {
		t.Fatal("partial scan accepted")
	}
	r.Scan.Coverage = security.CoverageComplete
	now := time.Now()
	r.Scan.FinishedAt = &now
	if _, err := Evaluate(r, p); err != nil {
		t.Fatal(err)
	}
	p.RequireSignature = true
	if err := ValidatePolicy(p); err == nil {
		t.Fatal("mandatory signature without trust accepted")
	}
	p.TrustedIdentity = "builder@example.com"
	p.TrustedIssuer = "https://accounts.google.com"
	if _, err := Evaluate(r, p); err == nil {
		t.Fatal("missing signature accepted")
	}
	p.Mode = "audit"
	if ds, err := Evaluate(r, p); err != nil || len(ds) == 0 {
		t.Fatalf("audit should report without blocking: %v", err)
	}
}

func TestStorePreservesNewerScanAndSigningBundles(t *testing.T) {
	s := NewStore(t.TempDir())
	original := populatedReport()
	if err := GenerateDocuments(s, original); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(original); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Load(original.AppID, Reference(original))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := s.PutDocument("cosign-bundle", []byte(`{"verificationMaterial":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	original.Signature = Signature{Status: "verified", Bundles: []Document{bundle}}
	original.Scan = &security.ScanRecord{Status: security.ScanCompleted, Coverage: security.CoverageComplete, Freshness: security.FreshnessFresh, FinishedAt: &now}
	if err := s.Save(original); err != nil {
		t.Fatal(err)
	}
	earlier := now.Add(-time.Hour)
	stale.Scan = &security.ScanRecord{Status: security.ScanCompleted, FinishedAt: &earlier}
	if err := s.Save(stale); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Load(original.AppID, Reference(original))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Scan == nil || !stored.Scan.FinishedAt.Equal(now) || len(stored.Signature.Bundles) != 1 {
		t.Fatalf("stale write discarded refreshed results: %+v", stored)
	}
	stored.Version = original.Version
	stored.Provenance.SHA256 = Digest([]byte("different build documents"))
	if err := s.Save(stored); err == nil {
		t.Fatal("persisted build documents could be replaced")
	}
}

func TestVerificationRefreshesScanAndRejectsDeletedSidecar(t *testing.T) {
	s := NewStore(t.TempDir())
	r := populatedReport()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := GenerateDocuments(s, r); err != nil {
		t.Fatal(err)
	}
	stale := *r
	now := time.Now().UTC()
	r.Scan = &security.ScanRecord{Status: security.ScanCompleted, Coverage: security.CoverageComplete, Freshness: security.FreshnessFresh, FinishedAt: &now}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.RequireScan = true
	if err := s.SavePolicy(r.AppID, p); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), s, &stale, path); err != nil {
		t.Fatalf("verification ignored current rescan: %v", err)
	}
	raw, err := json.Marshal(map[string]any{"versions": []any{map[string]any{"version": r.Version, "sha256": r.Artifact.SHA256, "security": r}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(s.DataDir, "apps", r.AppName, "versions.json"), raw); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApplicationPath(context.Background(), s, path, r.AppID, r.AppName); err == nil {
		t.Fatal("deleting security sidecar bypassed recorded build trust")
	}
}

func TestMatrixBuildsWithIdenticalBytesKeepIndependentEvidence(t *testing.T) {
	s := NewStore(t.TempDir())
	r := populatedReport()
	for _, buildID := range []string{"go1.27-linux-amd64", "go1.28-linux-amd64"} {
		copy := *r
		copy.BuildID = buildID
		if err := GenerateDocuments(s, &copy); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(&copy); err != nil {
			t.Fatal(err)
		}
		selected, err := s.ForBuild(r.AppID, r.Version, r.Artifact.SHA256, buildID)
		if err != nil || selected.BuildID != buildID {
			t.Fatalf("combination selection failed: %+v %v", selected, err)
		}
	}
	if _, err := s.ForVersion(r.AppID, r.Version, r.Artifact.SHA256); err == nil {
		t.Fatal("identical artifact bytes silently selected another build's evidence")
	}
	reports, err := s.List(r.AppID, 20, 0)
	if err != nil || len(reports) != 2 || reports[0].Provenance.SHA256 == reports[1].Provenance.SHA256 {
		t.Fatalf("matrix provenance was deduplicated: %+v %v", reports, err)
	}
}

func TestRejectedFinalizationCanRetrySameArtifact(t *testing.T) {
	t.Setenv("PHELIX_SIGNING_KEY", "")
	t.Setenv("PHELIX_KEYLESS_SIGNING", "")
	s := NewStore(t.TempDir())
	r := fixtureReport()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.RequireSignature = true
	p.PublicKey = filepath.Join(t.TempDir(), "missing.pub")
	if err := s.SavePolicy(r.AppID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := Finalize(context.Background(), s, r, path, 1); err == nil {
		t.Fatal("mandatory unsigned artifact was accepted")
	}
	if count, err := s.Count(r.AppID); err != nil || count != 0 {
		t.Fatalf("rejected build left an immutable version report: %d %v", count, err)
	}
	p.Mode = "audit"
	if err := s.SavePolicy(r.AppID, p); err != nil {
		t.Fatal(err)
	}
	r.BuiltAt = r.BuiltAt.Add(time.Minute)
	if _, err := Finalize(context.Background(), s, r, path, 1); err != nil {
		t.Fatalf("corrected build could not retry same artifact version: %v", err)
	}
}
