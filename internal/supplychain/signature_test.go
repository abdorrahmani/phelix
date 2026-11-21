package supplychain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRealCosignSigningAndTrust(t *testing.T) {
	if os.Getenv("PHELIX_VALIDATE_COSIGN") != "1" {
		t.Skip("set PHELIX_VALIDATE_COSIGN=1 with Cosign installed to validate real signing")
	}
	t.Setenv("COSIGN_PASSWORD", "")
	dir := t.TempDir()
	key := filepath.Join(dir, "trusted")
	other := filepath.Join(dir, "untrusted")
	for _, prefix := range []string{key, other} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		command := exec.CommandContext(ctx, "cosign", "generate-key-pair", "--output-key-prefix", prefix)
		raw, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("temporary verification key generation failed: %v %s", err, raw)
		}
	}
	store := NewStore(t.TempDir())
	r := populatedReport()
	path := filepath.Join(dir, "binary")
	if err := os.WriteFile(path, []byte("artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := GenerateDocuments(store, r); err != nil {
		t.Fatal(err)
	}
	policy := DefaultPolicy()
	policy.Mode = "enforce"
	policy.RequireSignature = true
	policy.PublicKey = key + ".pub"
	if err := store.SavePolicy(r.AppID, policy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := Sign(ctx, store, r, path, key+".key"); err != nil {
		t.Fatal(err)
	}
	if r.Signature.Status != "verified" || len(r.Signature.Bundles) != 4 || r.Signature.KeyFingerprint == "" {
		t.Fatalf("signing did not verify artifact and all documents: %+v", r.Signature)
	}
	if err := Verify(ctx, store, r, path); err != nil {
		t.Fatal(err)
	}
	policy.PublicKey = other + ".pub"
	if err := store.SavePolicy(r.AppID, policy); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, store, r, path); err == nil {
		t.Fatal("signature accepted an unrelated trust key")
	}
	if err := os.WriteFile(path, []byte("different artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, store, r, path); err == nil {
		t.Fatal("signature accepted a substituted artifact")
	}
}
