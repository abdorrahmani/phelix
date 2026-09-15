package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/webhook"
	"google.golang.org/protobuf/encoding/protojson"
)

// seedWebhookApp registers an app in the in-memory manager pointing at a
// project directory with a phelix.yaml webhook section (the same seeding
// pattern the remote matrix tests use).
func seedWebhookApp(t *testing.T, yaml string) string {
	t.Helper()
	projDir := t.TempDir()
	if yaml != "" {
		if err := os.WriteFile(filepath.Join(projDir, project.FileName), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	am, ok := app.Manager.(*app.AppManager)
	if !ok {
		t.Fatal("app manager is not the real implementation")
	}
	saved := am.Apps
	am.Apps = map[string]*app.AppInfo{
		"app-1": {ID: "app-1", Name: "api", Directory: projDir, Language: "go"},
	}
	t.Cleanup(func() { am.Apps = saved })
	return projDir
}

func runWebhookRemote(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	return RemoteWebhookCommand(req)
}

func webhookRemoteReq(requestID, cmdType string, mutate func(*pb.MonitorCommandRequest)) *pb.MonitorCommandRequest {
	req := &pb.MonitorCommandRequest{RequestId: requestID, Type: cmdType, AppName: "api"}
	if mutate != nil {
		mutate(req)
	}
	return req
}

func wantWebhookSuccess(t *testing.T, result *pb.MonitorCommandResult) *pb.MonitorCommandResult {
	t.Helper()
	if result.GetStatus() != "success" {
		t.Fatalf("status = %s, error = %s (%s)", result.GetStatus(), result.GetError(), result.GetErrorCode())
	}
	return result
}

func wantWebhookError(t *testing.T, result *pb.MonitorCommandResult, code phelixerr.Code) {
	t.Helper()
	if result.GetStatus() != "error" {
		t.Fatalf("status = %s, want error", result.GetStatus())
	}
	if result.GetErrorCode() != string(code) {
		t.Fatalf("error_code = %s, want %s (%s)", result.GetErrorCode(), code, result.GetError())
	}
}

const enabledYAML = `name: api
port: 3000
# operator comment that must survive remote edits
webhook:
  enabled: true
  branch: main
  secret_env: PHELIX_WEBHOOK_SECRET
`

func TestRemoteWebhookConfig(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedWebhookApp(t, enabledYAML)
	t.Setenv("PHELIX_WEBHOOK_SECRET", "the-actual-secret-value")

	result := wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r1", phelixgrpc.WebhookCommandConfig, nil)))
	cfg := result.GetWebhookConfig()
	if cfg == nil || !cfg.GetEnabled() || cfg.GetBranch() != "main" ||
		cfg.GetSecretEnv() != "PHELIX_WEBHOOK_SECRET" || cfg.GetSecretEnvStatus() != "configured" {
		t.Fatalf("config = %+v", cfg)
	}

	// The variable's VALUE must never appear anywhere in the result.
	assertNoSecretLeak(t, result, "the-actual-secret-value")

	// An unwatched/missing variable reports "missing" — presence only.
	t.Setenv("PHELIX_WEBHOOK_SECRET", "")
	result = wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r2", phelixgrpc.WebhookCommandConfig, nil)))
	if result.GetWebhookConfig().GetSecretEnvStatus() != "missing" {
		t.Fatalf("secret_env_status = %q, want missing", result.GetWebhookConfig().GetSecretEnvStatus())
	}
}

func TestRemoteWebhookConfig_NoPhelixYaml(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedWebhookApp(t, "")
	result := wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r1", phelixgrpc.WebhookCommandConfig, nil)))
	cfg := result.GetWebhookConfig()
	if cfg == nil || cfg.GetEnabled() || cfg.GetBranch() != "" || cfg.GetSecretEnvStatus() != "" {
		t.Fatalf("config = %+v, want disabled empty state", cfg)
	}
}

func TestRemoteWebhookStatusAndHistory(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	seedWebhookApp(t, enabledYAML)

	// Seed the durable job store the local CLI (and the webhook daemon)
	// write: one active, one finished job.
	store := webhook.NewJobStore(webhookJobsDir(), webhook.DefaultJobRetention)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	active := &webhook.JobRecord{
		DeliveryID: "d-active", AppID: "app-1", AppName: "api", Branch: "main",
		Commit: "aaaa000000000000000000000000000000000000", Provider: "github",
		Status: webhook.StatusAccepted, Stage: webhook.StageQueue, AcceptedAt: 1,
	}
	if err := store.Create(active); err != nil {
		t.Fatal(err)
	}
	done := &webhook.JobRecord{
		DeliveryID: "d-done", AppID: "app-1", AppName: "api", Branch: "main",
		Commit: "bbbb000000000000000000000000000000000000", Provider: "github",
		Status: webhook.StatusAccepted, Stage: webhook.StageQueue, AcceptedAt: 2,
	}
	if err := store.Create(done); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSyncing(active.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBuilding(active.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkProgress(active.ID, webhook.StatusDeploying, webhook.StageDeploy); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(done.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSucceeded(done.ID, 7); err != nil {
		t.Fatal(err)
	}

	status := wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r1", phelixgrpc.WebhookCommandStatus, nil)))
	if jobs := status.GetWebhookJobs(); len(jobs) != 1 || jobs[0].GetJobId() != active.ID ||
		jobs[0].GetStatus() != webhook.StatusDeploying || jobs[0].GetStage() != webhook.StageDeploy {
		t.Fatalf("status jobs = %+v", jobs)
	}

	history := wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r2", phelixgrpc.WebhookCommandHistory, nil)))
	if jobs := history.GetWebhookJobs(); len(jobs) != 1 || jobs[0].GetJobId() != done.ID ||
		jobs[0].GetVersion() != 7 || jobs[0].GetCommit() != "bbbb000000000000000000000000000000000000" {
		t.Fatalf("history jobs = %+v", jobs)
	}

	// Bounded limit is honored (0 → default 20; explicit values pass through).
	limited := wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r3", phelixgrpc.WebhookCommandHistory, func(r *pb.MonitorCommandRequest) {
		r.Webhook = &pb.WebhookOptions{Limit: 1}
	})))
	if len(limited.GetWebhookJobs()) != 1 {
		t.Fatalf("limit 1 returned %d jobs", len(limited.GetWebhookJobs()))
	}

	// The app's stable ID resolves the same app (the preferred identity).
	byID := webhookRemoteReq("r4", phelixgrpc.WebhookCommandStatus, nil)
	byID.AppName = "app-1"
	idResult := wantWebhookSuccess(t, runWebhookRemote(byID))
	if len(idResult.GetWebhookJobs()) != 1 {
		t.Fatalf("status by app ID returned %d jobs, want 1", len(idResult.GetWebhookJobs()))
	}
}

func TestRemoteWebhookQueries_EmptyStore(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())
	seedWebhookApp(t, enabledYAML)

	for _, cmdType := range []string{phelixgrpc.WebhookCommandStatus, phelixgrpc.WebhookCommandHistory, phelixgrpc.WebhookCommandDeliveries} {
		result := wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r", cmdType, nil)))
		if len(result.GetWebhookJobs()) != 0 || len(result.GetWebhookDeliveries()) != 0 {
			t.Fatalf("%s on an empty store returned data: %+v", cmdType, result)
		}
	}
}

func TestRemoteWebhookDeliveries(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PHELIX_DATA_DIR", dataDir)
	seedWebhookApp(t, enabledYAML)

	ledger := webhook.NewDeliveryLedger(deliveriesLedgerPath(), webhook.DefaultLedgerCapacity)
	if err := ledger.Initialize(); err != nil {
		t.Fatal(err)
	}
	seen, err := ledger.SeenOrRecord("api", "d-1", "main", "cccc000000000000000000000000000000000000", webhook.ProviderGitHub)
	if err != nil || seen {
		t.Fatalf("seed delivery: seen=%v err=%v", seen, err)
	}

	result := wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r1", phelixgrpc.WebhookCommandDeliveries, nil)))
	ds := result.GetWebhookDeliveries()
	if len(ds) != 1 || ds[0].GetDeliveryId() != "d-1" || ds[0].GetBranch() != "main" ||
		ds[0].GetCommit() != "cccc000000000000000000000000000000000000" || ds[0].GetAppName() != "api" {
		t.Fatalf("deliveries = %+v", ds)
	}
	// Metadata only — no signature, body or header data exists to leak.
	assertNoSecretLeak(t, result, "anything-not-a-known-field")
}

func TestRemoteWebhookMutations(t *testing.T) {
	t.Setenv("PHELIX_DATA_DIR", t.TempDir())

	t.Run("enable requires branch and secret_env", func(t *testing.T) {
		seedWebhookApp(t, "name: api\n")
		result := runWebhookRemote(webhookRemoteReq("r1", phelixgrpc.WebhookCommandEnable, func(r *pb.MonitorCommandRequest) {
			r.Webhook = &pb.WebhookOptions{Enabled: true}
		}))
		wantWebhookError(t, result, phelixerr.CodeConfiguration)
	})

	t.Run("full configuration lifecycle persists and validates", func(t *testing.T) {
		projDir := seedWebhookApp(t, "name: api\nport: 3000\n# operator comment that must survive remote edits\n")
		setBranch := func(id, branch string) *pb.MonitorCommandResult {
			return runWebhookRemote(webhookRemoteReq(id, phelixgrpc.WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Branch: branch}
			}))
		}
		setSecret := func(id, env string) *pb.MonitorCommandResult {
			return runWebhookRemote(webhookRemoteReq(id, phelixgrpc.WebhookCommandSetSecretEnv, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{SecretEnv: env}
			}))
		}
		enable := func(id string) *pb.MonitorCommandResult {
			return runWebhookRemote(webhookRemoteReq(id, phelixgrpc.WebhookCommandEnable, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Enabled: true}
			}))
		}

		cfg := wantWebhookSuccess(t, setBranch("r1", "release/2.x")).GetWebhookConfig()
		if cfg.GetBranch() != "release/2.x" || cfg.GetEnabled() {
			t.Fatalf("after set_branch: %+v", cfg)
		}
		cfg = wantWebhookSuccess(t, setSecret("r2", "API_HOOK_SECRET")).GetWebhookConfig()
		if cfg.GetSecretEnv() != "API_HOOK_SECRET" {
			t.Fatalf("after set_secret_env: %+v", cfg)
		}
		cfg = wantWebhookSuccess(t, enable("r3")).GetWebhookConfig()
		if !cfg.GetEnabled() || cfg.GetBranch() != "release/2.x" {
			t.Fatalf("after enable: %+v", cfg)
		}

		// The same phelix.yaml the local webhook server reads — with the
		// operator comment and unrelated keys intact.
		raw, err := os.ReadFile(filepath.Join(projDir, project.FileName))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, want := range []string{"# operator comment that must survive remote edits", "port: 3000", "release/2.x", "API_HOOK_SECRET", "enabled: true"} {
			if !strings.Contains(text, want) {
				t.Fatalf("phelix.yaml lost %q:\n%s", want, text)
			}
		}
		loaded, err := project.Load(projDir)
		if err != nil {
			t.Fatalf("saved yaml no longer loads: %v", err)
		}
		if loaded.Webhook == nil || !loaded.Webhook.Enabled || loaded.Webhook.Branch != "release/2.x" ||
			loaded.Webhook.SecretEnv != "API_HOOK_SECRET" {
			t.Fatalf("loaded webhook = %+v", loaded.Webhook)
		}

		// Disable flips the flag and keeps the rest.
		cfg = wantWebhookSuccess(t, runWebhookRemote(webhookRemoteReq("r4", phelixgrpc.WebhookCommandDisable, nil))).GetWebhookConfig()
		if cfg.GetEnabled() || cfg.GetBranch() != "release/2.x" {
			t.Fatalf("after disable: %+v", cfg)
		}
	})

	t.Run("invalid values are rejected and nothing is written", func(t *testing.T) {
		projDir := seedWebhookApp(t, "name: api\n")
		before, _ := os.ReadFile(filepath.Join(projDir, project.FileName))

		for _, tc := range []struct {
			name string
			req  *pb.MonitorCommandRequest
		}{
			{"ref as branch", webhookRemoteReq("r1", phelixgrpc.WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Branch: "refs/heads/main"}
			})},
			{"shell metacharacters in branch", webhookRemoteReq("r2", phelixgrpc.WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Branch: "main; rm -rf /"}
			})},
			{"invalid env name", webhookRemoteReq("r3", phelixgrpc.WebhookCommandSetSecretEnv, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{SecretEnv: "1BAD-NAME"}
			})},
			{"export syntax as env name", webhookRemoteReq("r4", phelixgrpc.WebhookCommandSetSecretEnv, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{SecretEnv: "export PWNED=1"}
			})},
		} {
			t.Run(tc.name, func(t *testing.T) {
				result := runWebhookRemote(tc.req)
				if result.GetStatus() != "error" {
					t.Fatalf("status = %s, want error", result.GetStatus())
				}
			})
		}
		after, _ := os.ReadFile(filepath.Join(projDir, project.FileName))
		if string(before) != string(after) {
			t.Fatalf("rejected mutations changed phelix.yaml:\n%s", after)
		}
	})

	t.Run("malformed phelix.yaml fails closed", func(t *testing.T) {
		seedWebhookApp(t, "webhook:\n  enabled: true\n  branch: main\n  secret_env: X\nhealth:\n  endpoints:\n    - name: x\n      path: bad\n")
		result := runWebhookRemote(webhookRemoteReq("r1", phelixgrpc.WebhookCommandConfig, nil))
		wantWebhookError(t, result, phelixerr.CodeConfiguration)
	})

	t.Run("unknown app", func(t *testing.T) {
		seedWebhookApp(t, enabledYAML)
		req := webhookRemoteReq("r1", phelixgrpc.WebhookCommandConfig, nil)
		req.AppName = "ghost"
		wantWebhookError(t, runWebhookRemote(req), phelixerr.CodeNotFound)
	})

	t.Run("unknown command type", func(t *testing.T) {
		seedWebhookApp(t, enabledYAML)
		wantWebhookError(t, runWebhookRemote(webhookRemoteReq("r1", "webhook_nuke", nil)), phelixerr.CodeInvalidArgument)
	})

	t.Run("concurrent idempotent mutations leave a loadable file", func(t *testing.T) {
		projDir := seedWebhookApp(t, "name: api\nwebhook:\n  enabled: true\n  branch: main\n  secret_env: PHELIX_WEBHOOK_SECRET\n")
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = runWebhookRemote(webhookRemoteReq("r", phelixgrpc.WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
					r.Webhook = &pb.WebhookOptions{Branch: "main"}
				}))
			}()
		}
		wg.Wait()
		// The file still loads cleanly after the concurrent writes.
		cfg, err := project.Load(projDir)
		if err != nil {
			t.Fatalf("phelix.yaml corrupted by concurrent mutations: %v", err)
		}
		if cfg.Webhook == nil || cfg.Webhook.Branch != "main" {
			t.Fatalf("webhook section lost: %+v", cfg.Webhook)
		}
	})
}

// assertNoSecretLeak fails when the secret value appears anywhere in the
// serialized result.
func assertNoSecretLeak(t *testing.T, result *pb.MonitorCommandResult, secret string) {
	t.Helper()
	data, err := protojson.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("secret value leaked into the command result:\n%s", data)
	}
	if strings.Contains(result.GetError(), secret) {
		t.Fatalf("secret value leaked into the error message: %s", result.GetError())
	}
}
