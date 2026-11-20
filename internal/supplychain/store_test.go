package supplychain

import (
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
