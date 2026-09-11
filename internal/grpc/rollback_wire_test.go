package grpc

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"google.golang.org/protobuf/proto"
)

// Wire-boundary tests for the rollback contract (docs/rollback-backend-contract.md).
// These exercise the real proto serialization, not struct-to-struct copies: the
// backend implements against bytes on the wire, so the tests marshal and
// unmarshal exactly what ReportRollbackEvent would carry.

// TestRollbackEvent_AdditiveFieldsRoundTrip pins the additive contract fields
// (error_code, reason, verification, target_source, dry_run, preview, history,
// source): every field must survive a real marshal/unmarshal cycle, and an
// event that carries none of them (an older agent) must decode with the
// documented zero values.
func TestRollbackEvent_AdditiveFieldsRoundTrip(t *testing.T) {
	builtAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	ev := &pb.RollbackLifecycleEvent{
		ServerId:         "agent-1",
		CliAppId:         "app-uuid-1",
		ResolvedAppId:    "app-uuid-1",
		AppName:          "shop",
		DeploymentMode:   "blue-green",
		RollbackStrategy: "blue-green",
		CurrentVersion:   "v12",
		TargetVersion:    "v11",
		TargetTag:        "stable",
		CurrentStep:      "verify_failed",
		Success:          false,
		Message:          "rollback verification failed",
		Error:            "the application did not remain healthy",
		ErrorCode:        "ROLLBACK_VERIFY_FAILED",
		Reason:           "login endpoint returning 500",
		VerifyRequested:  true,
		VerifyDurationMs: 30000,
		VerifyStatus:     "failed",
		VerifyError:      "unhealthy during window",
		TargetSource:     "explicit",
		Source:           "manual",
		Preview: &pb.RollbackPreview{
			AppName:        "shop",
			CurrentVersion: 12,
			TargetVersion:  11,
			TargetTag:      "stable",
			TargetBuiltAt:  builtAt.UnixMilli(),
			Strategy:       "blue-green",
			Downtime:       false,
			Steps:          []string{"acquire deploy lock", "deploy v11 to green slot"},
			Warnings:       []string{"no env snapshot for v11"},
		},
		History: []*pb.RollbackHistoryEntry{
			{
				Time:           builtAt.UnixMilli(),
				FromVersion:    "v12",
				ToVersion:      "v11",
				Status:         "success",
				Mode:           "blue-green",
				Reason:         "login endpoint returning 500",
				Source:         "manual",
				VerifyStatus:   "passed",
				VerifyDuration: "30s",
			},
		},
		Timestamp: time.Now().UnixMilli(),
	}

	raw, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back pb.RollbackLifecycleEvent
	if err := proto.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if back.GetErrorCode() != "ROLLBACK_VERIFY_FAILED" {
		t.Errorf("error_code = %q, want ROLLBACK_VERIFY_FAILED", back.GetErrorCode())
	}
	if back.GetReason() != "login endpoint returning 500" {
		t.Errorf("reason = %q", back.GetReason())
	}
	if !back.GetVerifyRequested() || back.GetVerifyDurationMs() != 30000 {
		t.Errorf("verification = requested=%v duration=%dms", back.GetVerifyRequested(), back.GetVerifyDurationMs())
	}
	if back.GetVerifyStatus() != "failed" || back.GetVerifyError() != "unhealthy during window" {
		t.Errorf("verify outcome = %q / %q", back.GetVerifyStatus(), back.GetVerifyError())
	}
	if back.GetTargetSource() != "explicit" {
		t.Errorf("target_source = %q, want explicit", back.GetTargetSource())
	}
	if back.GetSource() != "manual" {
		t.Errorf("source = %q, want manual", back.GetSource())
	}
	if p := back.GetPreview(); p == nil || p.GetTargetVersion() != 11 || p.GetTargetBuiltAt() != builtAt.UnixMilli() {
		t.Errorf("preview lost data: %+v", p)
	}
	if len(back.GetPreview().GetSteps()) != 2 || len(back.GetPreview().GetWarnings()) != 1 {
		t.Errorf("preview steps/warnings = %v / %v", back.GetPreview().GetSteps(), back.GetPreview().GetWarnings())
	}
	if h := back.GetHistory(); len(h) != 1 || h[0].GetVerifyDuration() != "30s" || h[0].GetMode() != "blue-green" {
		t.Errorf("history lost data: %+v", h)
	}
}

// TestRollbackEvent_OldAgentShapeDecodesWithZeroValues is the other compat
// direction: an agent built before the additive fields existed sends an event
// the new contract must still interpret — verification absent, not "failed".
func TestRollbackEvent_OldAgentShapeDecodesWithZeroValues(t *testing.T) {
	ev := &pb.RollbackLifecycleEvent{
		ServerId:       "agent-1",
		AppName:        "shop",
		CurrentStep:    "complete",
		Success:        true,
		CurrentVersion: "v12",
		TargetVersion:  "v11",
	}
	raw, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back pb.RollbackLifecycleEvent
	if err := proto.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.GetVerifyRequested() {
		t.Error("verify_requested must decode false for events that never carried it")
	}
	if back.GetVerifyStatus() != "" || back.GetErrorCode() != "" || back.GetReason() != "" {
		t.Error("additive string fields must decode empty for old-agent events")
	}
	if back.GetPreview() != nil || len(back.GetHistory()) != 0 {
		t.Error("preview/history must decode nil for old-agent events")
	}
}

// TestRollbackReporter_VerificationEventSemantics pins the wire behavior of
// the terminal verification event: a failed or cancelled window reports
// error_code=ROLLBACK_VERIFY_FAILED (the execution itself committed — the
// earlier complete event stays success=true), a passed window reports no
// error code.
func TestRollbackReporter_VerificationEventSemantics(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	r := NewRollbackReporter("srv", "cli-id", "res-id", "shop", "blue-green", "blue-green", "v12", "v11", "")
	r.SetVerification(30 * time.Second)

	if !r.Event().GetVerifyRequested() || r.Event().GetVerifyDurationMs() != 30000 {
		t.Fatalf("SetVerification must mark requested=true and duration=30000ms, got %+v", r.Event())
	}

	r.EmitVerifyOutcome(deploy.RollbackVerifyFailed, "unhealthy")
	ev := r.Event()
	if ev.GetCurrentStep() != RollbackStepVerifyFailed {
		t.Errorf("step = %q, want %q", ev.GetCurrentStep(), RollbackStepVerifyFailed)
	}
	if ev.GetVerifyStatus() != "failed" || ev.GetVerifyError() != "unhealthy" {
		t.Errorf("status/error = %q / %q", ev.GetVerifyStatus(), ev.GetVerifyError())
	}
	if ev.GetErrorCode() != string(phelixerr.CodeRollbackVerifyFailed) {
		t.Errorf("error_code = %q, want ROLLBACK_VERIFY_FAILED", ev.GetErrorCode())
	}
	if ev.GetSuccess() {
		t.Error("a failed verification event must not claim success")
	}

	// A passed window clears any error code and reports verify_passed.
	r2 := NewRollbackReporter("srv", "cli-id", "res-id", "shop", "rolling", "rolling", "v9", "v8", "")
	r2.SetErrorCode("SOME_STALE_CODE")
	r2.EmitVerifyOutcome(deploy.RollbackVerifyPassed, "")
	if r2.Event().GetCurrentStep() != RollbackStepVerifyPassed || r2.Event().GetErrorCode() != "" {
		t.Errorf("passed outcome: step=%q code=%q", r2.Event().GetCurrentStep(), r2.Event().GetErrorCode())
	}
}

// TestRollbackReporter_ErrorCodeCarriesStructuredCode pins that failed
// terminal events carry the machine-readable phelix error code alongside the
// free-form error text — the backend classifies outcomes by code, never by
// parsing messages.
func TestRollbackReporter_ErrorCodeCarriesStructuredCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	r := NewRollbackReporter("srv", "cli-id", "res-id", "shop", "classic", "classic", "v3", "v2", "")
	r.SetErrorCode(string(phelixerr.CodeDeployLocked))
	r.SetReason("login endpoint returning 500") // pre-validated by deploy.ValidateRollbackReason
	r.Emit(RollbackStepFailed, false, "rollback failed", time.Second, "deploy lock held by rebuild")

	ev := r.Event()
	if ev.GetErrorCode() != "DEPLOY_LOCKED" {
		t.Errorf("error_code = %q, want DEPLOY_LOCKED", ev.GetErrorCode())
	}
	if ev.GetReason() != "login endpoint returning 500" {
		t.Errorf("reason = %q — the CLI pre-validates, but the field must carry exactly that text", ev.GetReason())
	}
	if ev.GetSessionToken() != "" {
		t.Error("session token must never be serialized into the event body")
	}
}

func TestRollbackReporter_RequestIDTypedAndMetadataFallback(t *testing.T) {
	r := NewRollbackReporter("srv", "cli", "resolved", "shop", "classic", "classic", "v2", "v1", "")
	r.SetRequestID("req-123")
	if r.Event().GetRequestId() != "req-123" || r.Event().GetMetadata()["request_id"] != "req-123" {
		t.Fatalf("request correlation typed=%q metadata=%q", r.Event().GetRequestId(), r.Event().GetMetadata()["request_id"])
	}

	legacy := NewRollbackReporter("srv", "cli", "resolved", "shop", "classic", "classic", "v2", "v1", "")
	legacy.SetMetadata("request_id", "legacy-123")
	if legacy.Event().GetRequestId() != "legacy-123" {
		t.Fatalf("metadata fallback did not populate typed field: %q", legacy.Event().GetRequestId())
	}
}

func TestRollbackReporter_EmitClearsStaleCodeAndEmitErrorSetsTerminalCode(t *testing.T) {
	r := NewRollbackReporter("srv", "cli", "resolved", "shop", "classic", "classic", "v2", "v1", "")
	r.SetErrorCode("STALE")
	r.Emit(RollbackStepInit, true, "start", 0, "")
	if r.Event().GetErrorCode() != "" {
		t.Fatalf("successful event retained stale error code %q", r.Event().GetErrorCode())
	}
	err := phelixerr.New(phelixerr.CodeDeployLocked, "busy")
	r.EmitError(RollbackStepFailed, "rollback failed", time.Second, err)
	if r.Event().GetErrorCode() != string(phelixerr.CodeDeployLocked) || r.Event().GetSuccess() {
		t.Fatalf("terminal event code=%q success=%v", r.Event().GetErrorCode(), r.Event().GetSuccess())
	}
}

// TestRollbackEvent_HistoryAndPreviewSanitized pins UTF-8 sanitization over
// the new nested structures: user-controlled reason/verify-error text and
// stored metadata must never produce an invalid-UTF-8 wire rejection. Each
// invalid byte is replaced, so two bad bytes become two replacement chars.
func TestRollbackEvent_HistoryAndPreviewSanitized(t *testing.T) {
	bad := "\xff\xfe reason"
	want := string(utf8.RuneError) + string(utf8.RuneError) + " reason"
	ev := &pb.RollbackLifecycleEvent{
		Reason:      bad,
		VerifyError: bad,
		Preview:     &pb.RollbackPreview{Steps: []string{bad}, Warnings: []string{bad}},
		History:     []*pb.RollbackHistoryEntry{{Reason: bad, VerifyError: bad}},
	}
	sanitizeEventStrings(ev)
	if ev.GetReason() != want {
		t.Errorf("reason not sanitized: %q", ev.GetReason())
	}
	if ev.GetPreview().GetSteps()[0] != want {
		t.Errorf("preview steps not sanitized: %q", ev.GetPreview().GetSteps()[0])
	}
	if ev.GetHistory()[0].GetReason() != want {
		t.Errorf("history reason not sanitized: %q", ev.GetHistory()[0].GetReason())
	}
}
