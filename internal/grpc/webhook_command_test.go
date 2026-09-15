package grpc

import (
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"google.golang.org/protobuf/proto"
)

func webhookReq(requestID, cmdType string, mutate func(*pb.MonitorCommandRequest)) *pb.MonitorCommandRequest {
	req := &pb.MonitorCommandRequest{
		RequestId: requestID,
		Type:      cmdType,
		AppName:   "api",
	}
	if mutate != nil {
		mutate(req)
	}
	return req
}

// ---------------------------------------------------------------------------
// Transport-level validation
// ---------------------------------------------------------------------------

func TestValidateWebhookCommand(t *testing.T) {
	valid := map[string]*pb.MonitorCommandRequest{
		WebhookCommandStatus:     webhookReq("r", WebhookCommandStatus, nil),
		WebhookCommandHistory:    webhookReq("r", WebhookCommandHistory, nil),
		WebhookCommandConfig:     webhookReq("r", WebhookCommandConfig, nil),
		WebhookCommandDeliveries: webhookReq("r", WebhookCommandDeliveries, nil),
		WebhookCommandEnable: webhookReq("r", WebhookCommandEnable, func(r *pb.MonitorCommandRequest) {
			r.Webhook = &pb.WebhookOptions{Enabled: true}
		}),
		WebhookCommandDisable: webhookReq("r", WebhookCommandDisable, func(r *pb.MonitorCommandRequest) {
			r.Webhook = &pb.WebhookOptions{Enabled: false}
		}),
		WebhookCommandSetBranch: webhookReq("r", WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
			r.Webhook = &pb.WebhookOptions{Branch: "release/2.x"}
		}),
		WebhookCommandSetSecretEnv: webhookReq("r", WebhookCommandSetSecretEnv, func(r *pb.MonitorCommandRequest) {
			r.Webhook = &pb.WebhookOptions{SecretEnv: "PHELIX_WEBHOOK_SECRET"}
		}),
	}
	for cmdType, req := range valid {
		if err := validateWebhookCommand(req); err != nil {
			t.Fatalf("%s: unexpected error: %v", cmdType, err)
		}
	}
	// Mutating/queries classification must match the same set.
	for cmdType := range valid {
		wantMutating := cmdType == WebhookCommandEnable || cmdType == WebhookCommandDisable ||
			cmdType == WebhookCommandSetBranch || cmdType == WebhookCommandSetSecretEnv
		if got := isMutatingWebhookCommand(cmdType); got != wantMutating {
			t.Fatalf("isMutatingWebhookCommand(%s) = %v", cmdType, got)
		}
	}

	invalid := map[string]string{
		"missing request_id":           "request_id",
		"missing app_name":             "app_name",
		"unknown type":                 "unknown webhook command type",
		"foreign strategy":             "must not carry rollback/rebuild/matrix options",
		"foreign matrix options":       "must not carry rollback/rebuild/matrix options",
		"foreign target":               "must not carry rollback/rebuild/matrix options",
		"foreign dry_run":              "must not carry rollback/rebuild/matrix options",
		"options on status":            "must not carry webhook options",
		"options on config":            "must not carry webhook options",
		"branch on history":            "accepts only the webhook.limit option",
		"limit on enable":              "accepts only the webhook.enabled option",
		"enable with false":            "webhook_enable carries enabled=false",
		"disable with true":            "webhook_disable carries enabled=true",
		"set_branch without value":     "webhook_set_branch requires webhook.branch",
		"set_secret_env without value": "webhook_set_secret_env requires webhook.secret_env",
		"set_branch with secret_env":   "accepts only the webhook.branch option",
		"negative limit":               "must be 0 (default 20) or positive",
		"limit too large":              "must not exceed 100",
		"matrix with webhook options":  "must not carry webhook options (webhook)",
	}
	build := func(name string) *pb.MonitorCommandRequest {
		switch name {
		case "missing request_id":
			return webhookReq("", WebhookCommandStatus, nil)
		case "missing app_name":
			req := webhookReq("r", WebhookCommandStatus, nil)
			req.AppName = ""
			return req
		case "unknown type":
			return webhookReq("r", "webhook_nuke", nil)
		case "foreign strategy":
			req := webhookReq("r", WebhookCommandStatus, nil)
			req.Strategy = "blue-green"
			return req
		case "foreign matrix options":
			req := webhookReq("r", WebhookCommandStatus, nil)
			req.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}}
			return req
		case "foreign target":
			req := webhookReq("r", WebhookCommandStatus, nil)
			req.Target = "v3"
			return req
		case "foreign dry_run":
			req := webhookReq("r", WebhookCommandStatus, nil)
			req.DryRun = true
			return req
		case "options on status":
			return webhookReq("r", WebhookCommandStatus, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Limit: 5}
			})
		case "options on config":
			return webhookReq("r", WebhookCommandConfig, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Branch: "main"}
			})
		case "branch on history":
			return webhookReq("r", WebhookCommandHistory, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Branch: "main"}
			})
		case "limit on enable":
			return webhookReq("r", WebhookCommandEnable, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Limit: 5}
			})
		case "enable with false":
			return webhookReq("r", WebhookCommandEnable, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Enabled: false}
			})
		case "disable with true":
			return webhookReq("r", WebhookCommandDisable, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Enabled: true}
			})
		case "set_branch without value":
			return webhookReq("r", WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{}
			})
		case "set_secret_env without value":
			return webhookReq("r", WebhookCommandSetSecretEnv, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{}
			})
		case "set_branch with secret_env":
			return webhookReq("r", WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Branch: "main", SecretEnv: "X"}
			})
		case "negative limit":
			return webhookReq("r", WebhookCommandHistory, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Limit: -1}
			})
		case "limit too large":
			return webhookReq("r", WebhookCommandDeliveries, func(r *pb.MonitorCommandRequest) {
				r.Webhook = &pb.WebhookOptions{Limit: 101}
			})
		case "matrix with webhook options":
			req := webhookReq("r", MatrixCommandBuild, func(r *pb.MonitorCommandRequest) {
				r.Matrix = &pb.MatrixOptions{GoVersions: []string{"1.22"}, Platforms: []string{"linux/amd64"}}
				r.Webhook = &pb.WebhookOptions{Branch: "main"}
			})
			return req
		}
		return nil
	}
	for name, wantSubstring := range invalid {
		t.Run(name, func(t *testing.T) {
			req := build(name)
			if name == "matrix with webhook options" {
				if err := validateMatrixCommand(req); err == nil || !strings.Contains(err.Error(), wantSubstring) {
					t.Fatalf("matrix validation err = %v, want %q", err, wantSubstring)
				}
				return
			}
			err := validateWebhookCommand(req)
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), wantSubstring) {
				t.Fatalf("err = %v, want it to contain %q", err, wantSubstring)
			}
		})
	}
}

func TestWebhookQueryLimit(t *testing.T) {
	if got := WebhookQueryLimit(0); got != 20 {
		t.Fatalf("default limit = %d, want 20", got)
	}
	if got := WebhookQueryLimit(50); got != 50 {
		t.Fatalf("limit 50 = %d", got)
	}
	if got := WebhookQueryLimit(500); got != 100 {
		t.Fatalf("limit 500 = %d, want capped 100", got)
	}
}

// ---------------------------------------------------------------------------
// Durable idempotency (same ledger architecture as rollback/matrix)
// ---------------------------------------------------------------------------

func newTestWebhookLedger(t *testing.T) *rollbackLedger {
	t.Helper()
	l := newRollbackLedger(filepath.Join(t.TempDir(), webhookLedgerFile), rollbackLedgerCapacity, "webhook", "note")
	if err := l.initialize(); err != nil {
		t.Fatal(err)
	}
	return l
}

func successWebhookResult(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
	return &pb.MonitorCommandResult{
		Status: "success",
		WebhookConfig: &pb.WebhookConfigState{
			Enabled: true, Branch: "main", SecretEnv: "PHELIX_WEBHOOK_SECRET", SecretEnvStatus: "configured",
		},
	}
}

func TestWebhookLedger_BeginCompleteReplay(t *testing.T) {
	l := newTestWebhookLedger(t)
	req := webhookReq("req-1", WebhookCommandEnable, func(r *pb.MonitorCommandRequest) {
		r.Webhook = &pb.WebhookOptions{Enabled: true}
	})

	outcome, replay, err := l.begin(req)
	if err != nil || outcome != rollbackBeginExecute || replay != nil {
		t.Fatalf("begin: outcome=%v replay=%v err=%v", outcome, replay, err)
	}
	// A duplicate while in progress answers UNAVAILABLE — never re-executes.
	if _, _, err := l.begin(req); err == nil {
		t.Fatal("duplicate in-progress begin must fail")
	}

	stored := successWebhookResult(req)
	if err := l.complete(req.GetRequestId(), stored); err != nil {
		t.Fatal(err)
	}
	if err := l.markDelivered(req.GetRequestId()); err != nil {
		t.Fatal(err)
	}

	// Same request_id + same payload → replay of the stored immutable result.
	outcome, replay, err = l.begin(req)
	if err != nil || outcome != rollbackBeginReplay {
		t.Fatalf("replay begin: outcome=%v err=%v", outcome, err)
	}
	if replay.GetWebhookConfig().GetBranch() != "main" || replay.GetStatus() != "success" {
		t.Fatalf("replayed result: %+v", replay)
	}
}

func TestWebhookLedger_DifferentPayloadSameRequestID(t *testing.T) {
	l := newTestWebhookLedger(t)
	req := webhookReq("req-1", WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
		r.Webhook = &pb.WebhookOptions{Branch: "main"}
	})
	if _, _, err := l.begin(req); err != nil {
		t.Fatal(err)
	}
	if err := l.complete("req-1", successWebhookResult(req)); err != nil {
		t.Fatal(err)
	}
	changed := webhookReq("req-1", WebhookCommandSetBranch, func(r *pb.MonitorCommandRequest) {
		r.Webhook = &pb.WebhookOptions{Branch: "dev"}
	})
	_, _, err := l.begin(changed)
	if err == nil || !phelixerr.IsCode(err, phelixerr.CodeAlreadyExists) {
		t.Fatalf("begin with different payload = %v, want ALREADY_EXISTS", err)
	}
}

func TestWebhookLedger_FingerprintCoversWebhookOptions(t *testing.T) {
	// The deterministic request fingerprint must cover the new webhook
	// payload: two requests differing only in webhook options must not share
	// an idempotency slot.
	a := webhookReq("r", WebhookCommandSetSecretEnv, func(r *pb.MonitorCommandRequest) {
		r.Webhook = &pb.WebhookOptions{SecretEnv: "A_SECRET_ENV_NAME"}
	})
	b := webhookReq("r", WebhookCommandSetSecretEnv, func(r *pb.MonitorCommandRequest) {
		r.Webhook = &pb.WebhookOptions{SecretEnv: "OTHER_ENV_NAME"}
	})
	fa, err := rollbackRequestFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := rollbackRequestFingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fa == fb {
		t.Fatal("fingerprint must distinguish different webhook payloads")
	}
}

// ---------------------------------------------------------------------------
// Wire round-trip (additive fields survive serialization)
// ---------------------------------------------------------------------------

func TestWebhookProtoRoundTrip(t *testing.T) {
	req := &pb.MonitorCommandRequest{
		RequestId: "req-rt",
		Type:      WebhookCommandSetBranch,
		AppName:   "api",
		Webhook:   &pb.WebhookOptions{Enabled: true, Branch: "release/2.x", SecretEnv: "PHELIX_WEBHOOK_SECRET", Limit: 42},
	}
	data, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var decodedReq pb.MonitorCommandRequest
	if err := proto.Unmarshal(data, &decodedReq); err != nil {
		t.Fatal(err)
	}
	if decodedReq.GetWebhook().GetBranch() != "release/2.x" ||
		decodedReq.GetWebhook().GetSecretEnv() != "PHELIX_WEBHOOK_SECRET" ||
		!decodedReq.GetWebhook().GetEnabled() || decodedReq.GetWebhook().GetLimit() != 42 {
		t.Fatalf("request round-trip lost webhook options: %+v", decodedReq.GetWebhook())
	}

	result := &pb.MonitorCommandResult{
		RequestId: "req-rt",
		Command:   WebhookCommandStatus,
		Status:    "success",
		WebhookJobs: []*pb.WebhookJob{{
			JobId: "wh_0011223344556677", DeliveryId: "d-1", AppId: "app-1", AppName: "api",
			Branch: "main", Commit: "abcdef1234567890", Provider: "github",
			Status: "health_checking", Stage: "health_check",
			AcceptedAt: 1, StartedAt: 2, Version: 18,
			ErrorCode: "GIT_SYNC_FAILED", ErrorMessage: "sanitized",
		}},
		WebhookConfig: &pb.WebhookConfigState{
			Enabled: true, Branch: "main", SecretEnv: "PHELIX_WEBHOOK_SECRET", SecretEnvStatus: "configured",
		},
		WebhookDeliveries: []*pb.WebhookDelivery{{
			DeliveryId: "d-1", AppName: "api", Branch: "main", Commit: "abcdef", Provider: "github", ReceivedAt: 3,
		}},
	}
	data, err = proto.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded pb.MonitorCommandResult
	if err := proto.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.GetWebhookJobs()) != 1 || decoded.GetWebhookJobs()[0].GetJobId() != "wh_0011223344556677" ||
		decoded.GetWebhookJobs()[0].GetVersion() != 18 || decoded.GetWebhookJobs()[0].GetStage() != "health_check" {
		t.Fatalf("result round-trip lost webhook jobs: %+v", decoded.GetWebhookJobs())
	}
	if decoded.GetWebhookConfig().GetSecretEnvStatus() != "configured" {
		t.Fatalf("result round-trip lost config: %+v", decoded.GetWebhookConfig())
	}
	if len(decoded.GetWebhookDeliveries()) != 1 || decoded.GetWebhookDeliveries()[0].GetDeliveryId() != "d-1" {
		t.Fatalf("result round-trip lost deliveries: %+v", decoded.GetWebhookDeliveries())
	}
}

func TestWebhookCapabilityRegistered(t *testing.T) {
	for _, cap := range AgentCapabilities() {
		if cap == CapabilityWebhookManagement {
			return
		}
	}
	t.Fatalf("capability %q not registered", CapabilityWebhookManagement)
}

func TestExecuteWebhookCommand_NoHandlerRegistered(t *testing.T) {
	orig := webhookHandler
	webhookHandler = nil
	t.Cleanup(func() { webhookHandler = orig })

	result := executeWebhookCommand(webhookReq("r", WebhookCommandConfig, nil))
	if result.GetStatus() != "error" || result.GetErrorCode() != string(phelixerr.CodeUnimplemented) {
		t.Fatalf("result = %+v, want UNIMPLEMENTED error", result)
	}
}

func TestExecuteWebhookCommand_PanicBecomesTerminalError(t *testing.T) {
	orig := webhookHandler
	webhookHandler = func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult { panic("boom") }
	t.Cleanup(func() { webhookHandler = orig })

	result := executeWebhookCommand(webhookReq("r", WebhookCommandConfig, nil))
	if result.GetStatus() != "error" || result.GetErrorCode() != string(phelixerr.CodeUnknown) {
		t.Fatalf("result = %+v, want UNKNOWN error from panic isolation", result)
	}
}

func TestFinalizeWebhookResult_StampsIdentityAndCode(t *testing.T) {
	req := webhookReq("req-f", WebhookCommandEnable, nil)
	result := finalizeWebhookResult(req, &pb.MonitorCommandResult{Status: "error", Error: "boom"})
	if result.RequestId != "req-f" || result.Command != WebhookCommandEnable || result.AppName != "api" ||
		result.Timestamp == 0 || result.ErrorCode != string(phelixerr.CodeUnknown) {
		t.Fatalf("finalized = %+v", result)
	}
}
