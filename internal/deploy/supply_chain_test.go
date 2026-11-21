package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/resources"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
)

type artifactSecurityManager struct{ app.AppManagerInterface }

func (artifactSecurityManager) ListApplications() []app.AppListItem {
	return []app.AppListItem{{ID: "app_registered", Name: "example"}}
}

func TestNamedLaunchersRejectMissingSecurityEvidence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(dir, "state"))
	previous := app.Manager
	app.Manager = artifactSecurityManager{previous}
	t.Cleanup(func() { app.Manager = previous })
	p := supplychain.DefaultPolicy()
	p.Mode = "enforce"
	p.RequireSBOM = false
	p.RequireProvenance = false
	p.RequireScan = true
	if err := supplychain.NewStore(server.DataDir()).SavePolicy("app_registered", p); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "started")
	bin := filepath.Join(dir, "candidate")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	proc, port, err := launchAppInstance(context.Background(), bin, nil, resources.Config{}, "example")
	if err == nil || !strings.Contains(err.Error(), "security metadata is missing") || proc != nil || port != 0 {
		t.Fatalf("named service accepted unverified legacy artifact: %v %d %v", proc, port, err)
	}
	worker, _, err := launchAppWorkerInstance(bin, nil, resources.Config{}, "0", "example")
	if err == nil || !strings.Contains(err.Error(), "security metadata is missing") || worker != nil {
		t.Fatalf("named worker accepted unverified legacy artifact: %v %v", worker, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("candidate started despite rejected security policy: %v", err)
	}
}

func TestMatrixVersionPreservesArtifactBytes(t *testing.T) {
	withTempHome(t)
	source := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(source, []byte("first artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	artifacts := []MatrixArtifact{{Platform: "linux/amd64", Version: "1.27", Binary: source, Status: "success"}}
	recorded, err := RecordMatrixBuild("history-test", "", "", artifacts, "", DefaultRetention{Max: 5}, &nopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if recorded.Version != 1 || artifacts[0].Binary == source {
		t.Fatalf("matrix version retained a mutable build output: %+v", artifacts)
	}
	if err := os.WriteFile(source, []byte("next artifact"), 0755); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(artifacts[0].Binary)
	if err != nil || string(stored) != "first artifact" {
		t.Fatalf("later build replaced historical artifact: %q %v", stored, err)
	}
	if err := os.WriteFile(artifacts[0].Binary, []byte("tampered"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := verifyNativeAppArtifact(context.Background(), "history-test", artifacts[0].Binary); err == nil {
		t.Fatal("missing sidecar bypassed recorded matrix checksum")
	}
}

func TestRejectedMatrixVersionLeavesNoPartialReports(t *testing.T) {
	withTempHome(t)
	t.Setenv("PHELIX_SIGNING_KEY", "")
	t.Setenv("PHELIX_KEYLESS_SIGNING", "")
	store := supplychain.NewStore(server.DataDir())
	p := supplychain.DefaultPolicy()
	p.Mode = "enforce"
	if err := store.SavePolicy("app_matrix", p); err != nil {
		t.Fatal(err)
	}
	var artifacts []MatrixArtifact
	for _, buildID := range []string{"first", "second"} {
		path := filepath.Join(t.TempDir(), "binary")
		if err := os.WriteFile(path, []byte(buildID), 0755); err != nil {
			t.Fatal(err)
		}
		r := &supplychain.Report{SchemaVersion: 1, AppID: "app_matrix", AppName: "matrix-retry", BuildID: buildID, BuiltAt: time.Now().UTC(), Artifact: supplychain.Artifact{Filename: "binary", SHA256: supplychain.Digest([]byte(buildID)), SizeBytes: int64(len(buildID))}, Inventory: &security.Inventory{Ecosystem: security.EcosystemGo, Level: security.InventoryResolved}, Policy: p, Signature: supplychain.Signature{Status: "missing"}}
		if buildID == "second" {
			r.Inventory.Level = security.InventoryDegraded
		}
		artifacts = append(artifacts, MatrixArtifact{Platform: "linux/amd64", Version: buildID, Binary: path, Status: "success", Security: r})
	}
	retry := append([]MatrixArtifact(nil), artifacts...)
	if _, err := RecordMatrixBuild("matrix-retry", "", "", artifacts, "", DefaultRetention{Max: 5}, &nopLogger{}); err == nil {
		t.Fatal("partially verified matrix version accepted")
	}
	if count, err := store.Count("app_matrix"); err != nil || count != 0 {
		t.Fatalf("failed matrix version left immutable partial reports: %d %v", count, err)
	}
	retry[1].Security.Inventory.Level = security.InventoryResolved
	if _, err := RecordMatrixBuild("matrix-retry", "", "", retry, "", DefaultRetention{Max: 5}, &nopLogger{}); err != nil {
		t.Fatalf("corrected matrix could not retry unchanged first artifact: %v", err)
	}
}
