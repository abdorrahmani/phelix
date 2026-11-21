package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/supplychain"
)

func TestRemoteLogsAreBoundedAndExcludeIncompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	raw := strings.Repeat("complete line\n", 10000) + "password=incomplete-secret"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	lines, truncated, err := readRemoteLog(path)
	if err != nil || !truncated || len(lines) != 100 {
		t.Fatalf("lines=%d truncated=%v error=%v", len(lines), truncated, err)
	}
	for _, line := range lines {
		if strings.Contains(line, "incomplete-secret") {
			t.Fatal("incomplete credential line disclosed")
		}
	}
}

func TestRemoteActivationRejectsMonitoringSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := securityTestDataDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"sessionID": "test", "token": "test", "scope": "agent", "expiresAt": time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	command := newRemoteSecurityCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"enable"})
	if err := command.Execute(); phelixerr.CodeOf(err) != phelixerr.CodePermissionDenied {
		t.Fatalf("monitoring session granted activation: %v", err)
	}
	if security.RemoteSecurityEnabled(dir) {
		t.Fatal("denied activation changed remote consent")
	}
}

func TestRemoteSecurityStatusWithoutProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	result := RemoteSecurityCommand(securityRequest("status", phelixgrpc.SecurityRemoteStatus, "", nil))
	if result.Status != "success" || len(result.SupplyChainJson) == 0 {
		t.Fatalf("status depends on project: %v", result)
	}
	state, err := security.LoadRemoteState(securityTestDataDir())
	if err != nil || state.Enabled {
		t.Fatalf("default remote status: %+v %v", state, err)
	}
}

func TestRemoteCancellationRequiresSameApplication(t *testing.T) {
	handler := newRemoteSecurityHandler()
	result := handler(securityRequest("cancel", phelixgrpc.SecurityOperationCancel, "another-app", nil))
	if result.Status != "error" {
		t.Fatalf("missing cancellation target accepted: %v", result)
	}
}

func TestRemoteArtifactPathUsesMatrixBuildID(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	dir, err := deploy.AppDataDir("matrixapp")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	versions := deploy.VersionsFile{Versions: []deploy.VersionMeta{{Version: 1, MatrixArtifacts: []deploy.MatrixArtifact{
		{SHA256: digest, Binary: "/first", Security: &supplychain.Report{BuildID: "go1.27-linux-amd64"}},
		{SHA256: digest, Binary: "/second", Security: &supplychain.Report{BuildID: "go1.28-linux-amd64"}},
	}}}}
	raw, err := json.Marshal(versions)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "versions.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	path, err := remoteArtifactPath(&securityTarget{appName: "matrixapp"}, &supplychain.Report{Version: 1, BuildID: "go1.28-linux-amd64", Artifact: supplychain.Artifact{SHA256: digest}})
	if err != nil || path != "/second" {
		t.Fatalf("matrix selection returned %q: %v", path, err)
	}
}
