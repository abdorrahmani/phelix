package supplychain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSecurityReviewLegacyScanPolicyRejectsMissingEvidence(t *testing.T) {
	s := NewStore(t.TempDir())
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.RequireSBOM = false
	p.RequireProvenance = false
	p.RequireScan = true
	if err := s.SavePolicy("legacy-app", p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("legacy-artifact"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPath(context.Background(), s, path, "legacy-app"); err == nil {
		t.Fatal("enforced scan policy accepted artifact without scan metadata")
	}
}
