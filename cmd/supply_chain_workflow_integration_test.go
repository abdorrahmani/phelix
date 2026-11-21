package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supplychain"
	"google.golang.org/protobuf/proto"
)

func TestSupplyChainNativeWorkflowIntegration(t *testing.T) {
	for _, language := range []string{"go", "rust"} {
		t.Run(language, func(t *testing.T) {
			previousState := app.StateDirForTesting()
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PHELIX_SIGNING_KEY", "")
			t.Setenv("PHELIX_KEYLESS_SIGNING", "")
			writeIntegrationFile(t, filepath.Join(home, ".config/go/telemetry/mode"), "off\n")
			root := t.TempDir()
			name := "integration" + language
			id := "11111111-1111-4111-8111-111111111111"
			writeIntegrationFile(t, filepath.Join(root, "phelix.yaml"), "name: "+name+"\n")
			prepareIntegrationDependencies(t, root, language)
			registry, err := json.Marshal(map[string]any{id: map[string]any{"id": id, "name": name, "language": language, "status": "stopped", "port": 8080, "directory": root}})
			if err != nil {
				t.Fatal(err)
			}
			dataDir := server.DataDir()
			writeIntegrationFile(t, filepath.Join(dataDir, "apps.json"), string(registry))
			if err := app.SetStateDirForTesting(dataDir); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.SetStateDirForTesting(previousState); err != nil {
					t.Error(err)
				}
			})
			osv := securityOSV(t, "high")
			defer osv.Close()
			t.Setenv("PHELIX_OSV_URL", osv.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			manager := builder.NewBuildManager()
			manager.SetContext(ctx)
			binary := filepath.Join(t.TempDir(), "binary")
			metrics, err := buildApplication(id, nil, manager, binary)
			if err != nil {
				t.Fatalf("real %s build: %v", language, err)
			}
			release, err := deploy.AcquireDeployLock(name, "build")
			if err != nil {
				t.Fatal(err)
			}
			recorded, err := deploy.RecordFreshBuild(name, id, binary, "", "integration", metrics, deploy.UnlimitedRetention{}, nil)
			release()
			if err != nil {
				t.Fatalf("version persistence: %v", err)
			}
			store := supplychain.NewStore(dataDir)
			report, err := store.ForVersion(id, recorded.Version, "")
			if err != nil {
				t.Fatal(err)
			}
			if report.Version != 1 || report.Builder.Language != language || report.Source.SHA256 == "" || report.Builder.CompilerVersion == "" {
				t.Fatalf("actual build facts missing: %+v", report)
			}
			var direct, transitive bool
			for _, module := range report.Inventory.Modules {
				if strings.Contains(module.Name, "direct") && module.Direct {
					direct = true
				}
				if strings.Contains(module.Name, "transitive") && !module.Direct {
					transitive = true
				}
			}
			if !direct || !transitive || len(report.Inventory.Edges) < 2 {
				t.Fatalf("resolved direct/transitive graph missing: %+v", report.Inventory)
			}
			if len(report.SBOMs) != 2 || report.Provenance.SHA256 == "" {
				t.Fatalf("version has no complete document set: %+v", report)
			}
			if err := supplychain.Verify(ctx, store, report, recorded.BinaryPath); err != nil {
				t.Fatalf("stored artifact verification: %v", err)
			}
			state, err := security.SetRemoteSecurity(dataDir, true)
			if err != nil {
				t.Fatal(err)
			}
			result := RemoteSecurityCommand(securityRequest("actual-detail-"+language, phelixgrpc.SecurityArtifactDetail, id, &pb.SecurityCommandOptions{Version: 1}))
			if result.GetStatus() != "success" {
				t.Fatalf("remote actual report: %v", result)
			}
			report.Documents = map[string][]byte{}
			refs := append(append([]supplychain.Document{}, report.SBOMs...), report.Provenance)
			for _, reference := range refs {
				body, err := store.Document(reference.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				report.Documents[reference.SHA256] = body
				if dir := os.Getenv("PHELIX_SUPPLY_CHAIN_FIXTURE_DIR"); dir != "" {
					if err := os.WriteFile(filepath.Join(dir, language+"-"+reference.Format+".json"), body, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			body, err := json.Marshal(supplyChainAnswer{Report: report, Remote: &state})
			if err != nil {
				t.Fatal(err)
			}
			event := &pb.MonitorEvent{ServerId: "integration-server", Payload: &pb.MonitorEvent_SupplyChainJson{SupplyChainJson: body}}
			encoded, err := proto.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if dir := os.Getenv("PHELIX_SUPPLY_CHAIN_FIXTURE_DIR"); dir != "" {
				if err := os.WriteFile(filepath.Join(dir, language+"-event.bin"), encoded, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := security.SetRemoteSecurity(dataDir, false); err != nil {
				t.Fatal(err)
			}
			if got := RemoteSecurityCommand(securityRequest("disabled-"+language, phelixgrpc.SecurityArtifactDetail, id, &pb.SecurityCommandOptions{Version: 1})); got.GetStatus() != "error" || got.GetErrorCode() != "SECURITY_REPORTING_DISABLED" {
				t.Fatalf("disabled server disclosed report: %v", got)
			}
			if err := os.WriteFile(recorded.BinaryPath, []byte("tampered"), 0755); err != nil {
				t.Fatal(err)
			}
			if _, _, err := deploy.VersionPaths(name, 1); err == nil {
				t.Fatal("historical version lookup accepted tampered artifact")
			}
		})
	}
}

func TestSupplyChainRemoteCancellationIntegration(t *testing.T) {
	previousState := app.StateDirForTesting()
	securityRemoteFixture(t, true)
	t.Cleanup(func() {
		if err := app.SetStateDirForTesting(previousState); err != nil {
			t.Error(err)
		}
	})
	store := supplychain.NewStore(securityTestDataDir())
	report := &supplychain.Report{SchemaVersion: 1, AppID: securityRemoteAppID, AppName: "remoteapp", Version: 1, BuiltAt: time.Now().UTC(), Artifact: supplychain.Artifact{Filename: "binary", SHA256: supplychain.Digest([]byte("artifact")), SizeBytes: 8}, Policy: supplychain.DefaultPolicy(), Signature: supplychain.Signature{Status: "missing"}, Inventory: &security.Inventory{Ecosystem: security.EcosystemGo, Level: security.InventoryResolved, Modules: []security.Module{{Name: "example.com/vuln", Key: "example.com/vuln@v1.0.0", Version: "v1.0.0", Direct: true, Resolved: true, Confidence: security.ConfidenceResolved, SourceKind: security.SourceGoModule, Matchable: true}}}}
	if err := store.Save(report); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	stop := make(chan struct{})
	osv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer osv.Close()
	defer close(stop)
	t.Setenv("PHELIX_OSV_URL", osv.URL)
	handler := newRemoteSecurityHandler()
	done := make(chan *pb.MonitorCommandResult, 1)
	go func() {
		done <- handler(securityRequest("cancellable-rescan", phelixgrpc.SecurityArtifactRescan, securityRemoteAppID, &pb.SecurityCommandOptions{Version: 1}))
	}()
	select {
	case <-started:
	case result := <-done:
		t.Fatalf("rescan never reached scanner: %v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("scanner request did not start")
	}
	result := handler(securityRequest("foreign-cancel", phelixgrpc.SecurityOperationCancel, "foreign-app", &pb.SecurityCommandOptions{ScanId: "cancellable-rescan"}))
	if result.Status != "error" {
		t.Fatal("another application cancelled the operation")
	}
	select {
	case result := <-done:
		t.Fatalf("foreign cancellation stopped rescan: %v", result)
	default:
	}
	result = handler(securityRequest("owner-cancel", phelixgrpc.SecurityOperationCancel, securityRemoteAppID, &pb.SecurityCommandOptions{ScanId: "cancellable-rescan"}))
	if result.Status != "success" {
		t.Fatalf("cancellation request rejected: %v", result)
	}
	select {
	case result := <-done:
		if result.Status != "error" || result.ErrorCode != "CANCELLED" {
			t.Fatalf("original operation lost cancellation result: %v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled operation kept running")
	}
	stored, err := store.ForVersion(securityRemoteAppID, 1, "")
	if err != nil || stored.Scan == nil || stored.Scan.Status != security.ScanFailed {
		t.Fatalf("cancelled scanner persisted a clean result: %+v %v", stored, err)
	}
	result = handler(securityRequest("late-cancel", phelixgrpc.SecurityOperationCancel, securityRemoteAppID, &pb.SecurityCommandOptions{ScanId: "cancellable-rescan"}))
	if result.Status != "error" {
		t.Fatal("completed operation remained in cancellation registry")
	}
}

func TestSupplyChainRemoteLogsRedactionIntegration(t *testing.T) {
	previousState := app.StateDirForTesting()
	securityRemoteFixture(t, false)
	t.Cleanup(func() {
		if err := app.SetStateDirForTesting(previousState); err != nil {
			t.Error(err)
		}
	})
	secret := "actual-integration-credential-value"
	phelixerr.RegisterSecretValue(secret)
	t.Cleanup(func() { phelixerr.UnregisterSecretValue(secret) })
	writeIntegrationFile(t, logs.AppLogPath(securityRemoteAppID), strings.Repeat("entry "+secret+" password=private-value\n", 1000)+strings.Repeat("x", 100000))
	result := RemoteSecurityCommand(securityRequest("bounded-logs", phelixgrpc.SecurityAppLogs, securityRemoteAppID, nil))
	if result.Status != "success" {
		t.Fatalf("approved application logs failed: %v", result)
	}
	var answer supplyChainAnswer
	if err := json.Unmarshal(result.SupplyChainJson, &answer); err != nil {
		t.Fatal(err)
	}
	if !answer.Truncated || len(answer.Output) > 65536 || strings.Contains(answer.Output, secret) || strings.Contains(answer.Output, "private-value") {
		t.Fatalf("remote logs were unbounded or disclosed credentials: %+v", answer)
	}
	writeIntegrationFile(t, logs.AppLogPath(securityRemoteAppID), "entry "+secret+" password=private-value\n")
	result = RemoteSecurityCommand(securityRequest("redacted-logs", phelixgrpc.SecurityAppLogs, securityRemoteAppID, nil))
	if result.Status != "success" {
		t.Fatal(result)
	}
	if err := json.Unmarshal(result.SupplyChainJson, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Output == "" || strings.Contains(answer.Output, secret) || strings.Contains(answer.Output, "private-value") {
		t.Fatalf("complete log credentials were disclosed: %+v", answer)
	}
}

func writeIntegrationFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func prepareIntegrationDependencies(t *testing.T, root, language string) {
	t.Helper()
	if language == "go" {
		writeIntegrationFile(t, filepath.Join(root, "go.mod"), "module integration/app\n\ngo 1.24\n\nrequire integration/direct v0.0.0\n\nreplace integration/direct => ./deps/direct\nreplace integration/transitive => ./deps/transitive\n")
		writeIntegrationFile(t, filepath.Join(root, "main.go"), "package main\nimport \"integration/direct\"\nfunc main() { println(direct.Value()) }\n")
		writeIntegrationFile(t, filepath.Join(root, "deps/direct/go.mod"), "module integration/direct\n\ngo 1.24\nrequire integration/transitive v0.0.0\n")
		writeIntegrationFile(t, filepath.Join(root, "deps/direct/direct.go"), "package direct\nimport \"integration/transitive\"\nfunc Value() string { return transitive.Value() }\n")
		writeIntegrationFile(t, filepath.Join(root, "deps/transitive/go.mod"), "module integration/transitive\n\ngo 1.24\n")
		writeIntegrationFile(t, filepath.Join(root, "deps/transitive/transitive.go"), "package transitive\nfunc Value() string { return \"actual dependency graph\" }\n")
		cmd := exec.Command("go", "mod", "tidy")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOPROXY=off")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Go graph resolution: %v\n%s", err, output)
		}
		return
	}
	writeIntegrationFile(t, filepath.Join(root, "Cargo.toml"), "[package]\nname = \"integration-app\"\nversion = \"0.1.0\"\nedition = \"2021\"\nlicense = \"MIT\"\n[dependencies]\ndirect = { path = \"deps/direct\" }\n")
	writeIntegrationFile(t, filepath.Join(root, "src/main.rs"), "fn main() { println!(\"{}\", direct::value()); }\n")
	writeIntegrationFile(t, filepath.Join(root, "deps/direct/Cargo.toml"), "[package]\nname = \"direct\"\nversion = \"0.1.0\"\nedition = \"2021\"\nlicense = \"MIT OR Apache-2.0\"\n[dependencies]\ntransitive = { path = \"../transitive\" }\n")
	writeIntegrationFile(t, filepath.Join(root, "deps/direct/src/lib.rs"), "pub fn value() -> &'static str { transitive::value() }\n")
	writeIntegrationFile(t, filepath.Join(root, "deps/transitive/Cargo.toml"), "[package]\nname = \"transitive\"\nversion = \"0.1.0\"\nedition = \"2021\"\nlicense = \"Apache-2.0\"\n")
	writeIntegrationFile(t, filepath.Join(root, "deps/transitive/src/lib.rs"), "pub fn value() -> &'static str { \"actual dependency graph\" }\n")
	cmd := exec.Command("cargo", "generate-lockfile", "--offline")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Cargo graph resolution: %v\n%s", err, output)
	}
}
