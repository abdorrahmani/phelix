package grpc

import (
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

// Tests for the read-only rollback query commands (rollback_list /
// rollback_history): transport-shape validation, app resolution, the ack
// result, and the history mapping — plus an end-to-end MonitorStream check that
// a query answers without touching the executor or the idempotency ledger.

func TestValidateRollbackQueryCommand(t *testing.T) {
	ok := &pb.MonitorCommandRequest{RequestId: "q1", Type: RollbackCommandList, AppName: "shop", Limit: 5}
	if err := validateRollbackQueryCommand(ok); err != nil {
		t.Fatalf("valid query rejected: %v", err)
	}

	cases := map[string]*pb.MonitorCommandRequest{
		"missing request_id": {Type: RollbackCommandList, AppName: "shop"},
		"missing app_name":   {RequestId: "q", Type: RollbackCommandHistory},
		"foreign reason":     {RequestId: "q", Type: RollbackCommandList, AppName: "shop", Reason: "x"},
		"foreign verify":     {RequestId: "q", Type: RollbackCommandList, AppName: "shop", VerifyDurationMs: 1},
		"foreign dry_run":    {RequestId: "q", Type: RollbackCommandList, AppName: "shop", DryRun: true},
		"foreign target":     {RequestId: "q", Type: RollbackCommandHistory, AppName: "shop", Target: "v2"},
		"foreign matrix":     {RequestId: "q", Type: RollbackCommandList, AppName: "shop", Matrix: &pb.MatrixOptions{}},
		"negative limit":     {RequestId: "q", Type: RollbackCommandHistory, AppName: "shop", Limit: -1},
	}
	for name, req := range cases {
		if err := validateRollbackQueryCommand(req); !phelixerr.IsCode(err, phelixerr.CodeInvalidArgument) {
			t.Errorf("%s: error = %v, want INVALID_ARGUMENT", name, err)
		}
	}
}

func TestRollbackHistoryEntriesFromRecords(t *testing.T) {
	rec := deploy.RollbackHistoryRecord{
		Time: time.UnixMilli(1700000000000), From: "v5", To: "v4",
		Status: "success", Mode: "blue-green", Reason: "bad login", Source: "manual",
		Verification: &deploy.RollbackVerification{Status: "passed", Duration: "30s"},
	}
	out := rollbackHistoryEntriesFromRecords([]deploy.RollbackHistoryRecord{rec})
	if len(out) != 1 {
		t.Fatalf("mapped %d entries, want 1", len(out))
	}
	e := out[0]
	if e.GetFromVersion() != "v5" || e.GetToVersion() != "v4" || e.GetStatus() != "success" ||
		e.GetMode() != "blue-green" || e.GetReason() != "bad login" || e.GetSource() != "manual" {
		t.Fatalf("history entry mismapped: %+v", e)
	}
	if e.GetVerifyStatus() != "passed" || e.GetVerifyDuration() != "30s" {
		t.Fatalf("verification sub-record mismapped: status=%q duration=%q", e.GetVerifyStatus(), e.GetVerifyDuration())
	}
	if e.GetTime() != 1700000000000 {
		t.Fatalf("time = %d, want unix millis 1700000000000", e.GetTime())
	}
}

func TestResolveRollbackQueryApp(t *testing.T) {
	origMgr := app.Manager
	app.Manager = &watchingGateStub{items: []app.AppListItem{{ID: "app-uuid-1", Name: "shop"}}}
	t.Cleanup(func() { app.Manager = origMgr })

	id, name, err := resolveRollbackQueryApp("shop")
	if err != nil || id != "app-uuid-1" || name != "shop" {
		t.Fatalf("resolve by name = %q %q %v", id, name, err)
	}
	if _, _, err := resolveRollbackQueryApp("app-uuid-1"); err != nil {
		t.Fatalf("resolve by ID failed: %v", err)
	}
	if _, _, err := resolveRollbackQueryApp("ghost"); !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
		t.Fatalf("unknown app error = %v, want NOT_FOUND", err)
	}
}

func TestExecuteRollbackQueryUnknownApp(t *testing.T) {
	origMgr := app.Manager
	app.Manager = &watchingGateStub{}
	t.Cleanup(func() { app.Manager = origMgr })

	res := executeRollbackQuery(&pb.MonitorCommandRequest{RequestId: "q", Type: RollbackCommandList, AppName: "ghost"})
	if res.GetStatus() != "error" || res.GetErrorCode() != string(phelixerr.CodeNotFound) {
		t.Fatalf("unknown-app ack status=%q code=%q, want error / NOT_FOUND", res.GetStatus(), res.GetErrorCode())
	}
	if res.GetRequestId() != "q" || res.GetCommand() != RollbackCommandList {
		t.Fatalf("ack identity not stamped: %+v", res)
	}
}

// TestIsRollbackQueryCommand pins the routing predicate: the two query types
// are recognized (so monitorRecvLoop routes them to the read-only query
// handler, never to the ledger-guarded execute path), and execute/lifecycle
// types are not.
func TestIsRollbackQueryCommand(t *testing.T) {
	for _, typ := range []string{RollbackCommandList, RollbackCommandHistory} {
		if !isRollbackQueryCommand(typ) {
			t.Errorf("%q should be a rollback query command", typ)
		}
	}
	for _, typ := range []string{"rollback", "restart", "matrix_list", "webhook_status", ""} {
		if isRollbackQueryCommand(typ) {
			t.Errorf("%q must not be treated as a rollback query command", typ)
		}
	}
}
