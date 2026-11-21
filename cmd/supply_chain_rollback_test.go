package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/supplychain"
)

func TestRollbackPreservesSecurityMetadata(t *testing.T) {
	for _, existed := range []bool{true, false} {
		t.Run(map[bool]string{true: "existing binary", false: "new binary"}[existed], func(t *testing.T) {
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "target"), filepath.Join(dir, "live")
			write := func(path, value string, version int) {
				t.Helper()
				if err := os.WriteFile(path, []byte(value), 0755); err != nil {
					t.Fatal(err)
				}
				r := &supplychain.Report{SchemaVersion: 1, AppID: "app_test", AppName: "example", Version: version, BuiltAt: time.Now().UTC(), Artifact: supplychain.Artifact{Filename: filepath.Base(path), SHA256: supplychain.Digest([]byte(value)), SizeBytes: int64(len(value))}, Inventory: &security.Inventory{Ecosystem: security.EcosystemGo, Level: security.InventoryResolved}, Policy: supplychain.DefaultPolicy(), Signature: supplychain.Signature{Status: "missing"}}
				if err := supplychain.SaveSidecar(path, r); err != nil {
					t.Fatal(err)
				}
			}
			write(src, "new", 2)
			if existed {
				write(dst, "old", 1)
			}
			backup, err := replaceBinaryForRollback(src, dst)
			if err != nil {
				t.Fatal(err)
			}
			replaced, err := supplychain.LoadSidecar(dst)
			if err != nil || replaced.Version != 2 {
				t.Fatalf("replacement metadata mismatch: %+v %v", replaced, err)
			}
			if err := backup.Restore(); err != nil {
				t.Fatal(err)
			}
			if existed {
				restored, err := supplychain.LoadSidecar(dst)
				if err != nil || restored.Version != 1 {
					t.Fatalf("prior metadata was not restored: %+v %v", restored, err)
				}
			} else {
				for _, path := range []string{dst, dst + ".supply-chain.json"} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("failed fresh deployment retained %s: %v", path, err)
					}
				}
			}
		})
	}
}

func TestRollbackRejectsInvalidMetadataBeforeReplacingBinary(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "target"), filepath.Join(dir, "live")
	for path, value := range map[string]string{src: "new", dst: "old", src + ".supply-chain.json": "{"} {
		if err := os.WriteFile(path, []byte(value), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyFileForRollback(src, dst); err == nil {
		t.Fatal("invalid metadata accepted")
	}
	raw, err := os.ReadFile(dst)
	if err != nil || string(raw) != "old" {
		t.Fatalf("failed metadata validation replaced live binary: %q %v", raw, err)
	}
}
