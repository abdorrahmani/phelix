package cmd

import (
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
)

// These tests pin the CLI side of the rollback telemetry contract: what the
// CLI actually puts on the wire (docs/rollback-backend-contract.md). The
// emitters build the proto messages the backend will receive; the assertions
// check the wire-shaped values, not internal structs.

// TestTagForVersion pins that the resolved target's tag — never the raw --to
// input — feeds target_tag on rollback events.
func TestTagForVersion(t *testing.T) {
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)

	if got := deploy.TagForVersion("shop", 2); got != "stable" {
		t.Errorf("TagForVersion(v2) = %q, want stable", got)
	}
	if got := deploy.TagForVersion("shop", 1); got != "" {
		t.Errorf("untagged v1 = %q, want empty", got)
	}
	if got := deploy.TagForVersion("shop", 99); got != "" {
		t.Errorf("unknown v99 = %q, want empty", got)
	}
}

// TestBuildRollbackPreview_MapsPlanFields pins the plan → wire mapping used
// by dry-run events, including the unix-milli timestamp conversion and the
// deliberate omission of local filesystem paths.
func TestBuildRollbackPreview_MapsPlanFields(t *testing.T) {
	builtAt := time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC)
	plan := &deploy.RollbackPlan{
		AppName:              "shop",
		CurrentVersion:       3,
		TargetVersion:        2,
		TargetTag:            "stable",
		TargetCommit:         "bbbb2222bbbb",
		TargetBuiltAt:        builtAt,
		TargetSize:           11 * 1024 * 1024,
		CurrentTag:           "",
		CurrentCommit:        "cccc3333cccc",
		CurrentSize:          12 * 1024 * 1024,
		BinaryPath:           "/home/x/.phelix/apps/shop/builds/v2/binary",
		EnvSnapshotAvailable: true,
		EnvSummary:           "snapshot available (v2)",
		Strategy:             string(deploy.ModeBlueGreen),
		Replicas:             0,
		PublicPort:           3000,
		CurrentSlot:          "green",
		TargetSlot:           "blue",
		CurrentPort:          41001,
		HealthCheck:          "tier 2 http",
		Downtime:             false,
		Steps:                []string{"step one", "step two"},
		Warnings:             []string{"no env snapshot"},
	}

	p := buildRollbackPreview("shop", plan)

	if p.GetAppName() != "shop" || p.GetCurrentVersion() != 3 || p.GetTargetVersion() != 2 {
		t.Fatalf("identity/version mapping wrong: %+v", p)
	}
	if p.GetTargetBuiltAt() != builtAt.UnixMilli() {
		t.Errorf("target_built_at = %d, want %d", p.GetTargetBuiltAt(), builtAt.UnixMilli())
	}
	if p.GetStrategy() != "blue-green" || p.GetTargetSlot() != "blue" || p.GetDowntime() {
		t.Errorf("strategy mapping wrong: %+v", p)
	}
	if len(p.GetSteps()) != 2 || len(p.GetWarnings()) != 1 {
		t.Errorf("steps/warnings not carried: %v / %v", p.GetSteps(), p.GetWarnings())
	}
}

// TestRollbackHistoryEntries_MapsRecords pins the history record → wire
// mapping: execution status stays separate from the verification outcome and
// the mode records what actually happened.
func TestRollbackHistoryEntries_MapsRecords(t *testing.T) {
	at := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	records := []deploy.RollbackHistoryRecord{
		{
			Time: at, App: "shop", From: "v12", To: "v11",
			Status: deploy.RollbackStatusSuccess, Mode: "blue-green",
			Reason: "bad release", Source: deploy.RollbackSourceManual,
			Verification: &deploy.RollbackVerification{
				Requested: true, Duration: "30s", Status: deploy.RollbackVerifyFailed,
				Error: "unhealthy", At: at,
			},
		},
		{
			Time: at.Add(time.Minute), App: "shop", From: "v10", To: "v9",
			Status: deploy.RollbackStatusFailed, Mode: "classic",
			Source: deploy.RollbackSourceAutomatic, Error: "start failed",
		},
	}

	entries := rollbackHistoryEntries(records)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	first, second := entries[0], entries[1]

	if first.GetTime() != at.UnixMilli() || first.GetFromVersion() != "v12" || first.GetToVersion() != "v11" {
		t.Errorf("record 1 identity wrong: %+v", first)
	}
	if first.GetStatus() != "success" || first.GetMode() != "blue-green" || first.GetSource() != "manual" {
		t.Errorf("record 1 outcome wrong: %+v", first)
	}
	if first.GetVerifyStatus() != "failed" || first.GetVerifyDuration() != "30s" || first.GetVerifyError() != "unhealthy" {
		t.Errorf("record 1 verification wrong: %+v", first)
	}
	if second.GetVerifyStatus() != "" || second.GetSource() != "automatic" || second.GetError() != "start failed" {
		t.Errorf("record 2 wrong: %+v", second)
	}
}

// TestBuildAutoRollbackEvent pins that automatic recoveries now reach the
// wire with source=automatic, the structured error code on failure, and the
// already-serving nuance in the message.
func TestBuildAutoRollbackEvent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)
	appInfo := &app.AppInfo{ID: "app-uuid-1", Name: "shop"}

	ok := buildAutoRollbackEvent(appInfo, "shop", "blue-green", 3, 2, "Deployment v3 failed: health check", false, nil)
	if ok.GetSource() != "automatic" || ok.GetTargetSource() != "automatic" {
		t.Errorf("success event source wrong: source=%q target_source=%q", ok.GetSource(), ok.GetTargetSource())
	}
	if ok.GetCurrentStep() != "complete" || ok.GetTargetTag() != "stable" {
		t.Errorf("success event wrong: step=%q target_tag=%q", ok.GetCurrentStep(), ok.GetTargetTag())
	}
	if !ok.GetSuccess() {
		t.Error("restored recovery must be a success event")
	}

	serving := buildAutoRollbackEvent(appInfo, "shop", "blue-green", 3, 2, "Deployment v3 failed", true, nil)
	if !serving.GetSuccess() || serving.GetMessage() == "" {
		t.Errorf("already-serving event wrong: %+v", serving)
	}

	// The empty mode case (deploy state unreadable) still records the mode
	// honestly as empty rather than a fabricated value.
	failed := buildAutoRollbackEvent(appInfo, "shop", "", 3, 0, "Deployment v3 failed", false,
		phelixerr.New(phelixerr.CodeRollbackTargetNotFound, "no known-good version"))
	if failed.GetSuccess() || failed.GetErrorCode() != string(phelixerr.CodeRollbackTargetNotFound) {
		t.Errorf("failure event wrong: success=%v error_code=%q", failed.GetSuccess(), failed.GetErrorCode())
	}
	if failed.GetTargetVersion() != "" {
		t.Errorf("unresolved target must be empty on the wire, got %q", failed.GetTargetVersion())
	}
}

// TestRollbackListEventNoTargetTag pins the --list reporter fix: the raw --to
// input never leaks into target_tag on list events.
func TestRollbackListEventNoTargetTag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	seedRollbackPreviewApp(t, "shop", deploy.ModeBlueGreen)
	appInfo := &app.AppInfo{ID: "app-uuid-1", Name: "shop"}

	// Mirrors the listRollbackVersions construction: mode=strategy="list"
	// and no target version/tag — list is not an execution event.
	r := phelixgrpc.NewRollbackReporter("", appInfo.ID, appInfo.ID, "shop", "list", "list", "", "", "")
	r.SetMetadata("version_count", "3")
	ev := r.Event()
	if ev.GetTargetTag() != "" {
		t.Errorf("list event target_tag = %q, want empty", ev.GetTargetTag())
	}
	if ev.GetTargetSource() != "" || ev.GetSource() != "" {
		t.Errorf("list event is not an execution: target_source=%q source=%q", ev.GetTargetSource(), ev.GetSource())
	}
}
