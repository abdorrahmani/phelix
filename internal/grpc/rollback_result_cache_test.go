package grpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func testRollbackLedger(t *testing.T, max int) *rollbackLedger {
	t.Helper()
	l := newRollbackLedger(filepath.Join(t.TempDir(), rollbackLedgerFile), max, "rollback", "")
	if err := l.initialize(); err != nil {
		t.Fatalf("initialize ledger: %v", err)
	}
	return l
}

func rollbackRequest(id, target string) *pb.MonitorCommandRequest {
	return &pb.MonitorCommandRequest{RequestId: id, Type: "rollback", AppName: "shop", Target: target}
}

func TestRollbackLedgerSamePayloadReplays(t *testing.T) {
	l := testRollbackLedger(t, 4)
	req := rollbackRequest("r1", "v2")
	if outcome, _, err := l.begin(req); err != nil || outcome != rollbackBeginExecute {
		t.Fatalf("first begin = %v, %v", outcome, err)
	}
	want := &pb.MonitorCommandResult{RequestId: "r1", Command: "rollback", Status: "success"}
	if err := l.complete("r1", want); err != nil {
		t.Fatalf("complete: %v", err)
	}
	outcome, got, err := l.begin(req)
	if err != nil || outcome != rollbackBeginReplay || got.GetStatus() != "success" {
		t.Fatalf("replay = outcome %v result %+v err %v", outcome, got, err)
	}
}

func TestRollbackLedgerFingerprintExcludesOnlyRequestID(t *testing.T) {
	a, err := rollbackRequestFingerprint(rollbackRequest("a", "v2"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := rollbackRequestFingerprint(rollbackRequest("b", "v2"))
	c, _ := rollbackRequestFingerprint(rollbackRequest("a", "v3"))
	if a != b {
		t.Fatal("request_id must not affect fingerprint")
	}
	if a == c {
		t.Fatal("payload change must affect fingerprint")
	}
}

func TestRollbackLedgerSameIDDifferentPayloadRejected(t *testing.T) {
	l := testRollbackLedger(t, 4)
	if _, _, err := l.begin(rollbackRequest("r1", "v2")); err != nil {
		t.Fatal(err)
	}
	_, _, err := l.begin(rollbackRequest("r1", "v3"))
	if !phelixerr.IsCode(err, phelixerr.CodeAlreadyExists) {
		t.Fatalf("error = %v, want ALREADY_EXISTS", err)
	}
}

func TestRollbackLedgerCapacityRejectsWithoutEviction(t *testing.T) {
	l := testRollbackLedger(t, 1)
	if _, _, err := l.begin(rollbackRequest("r1", "v2")); err != nil {
		t.Fatal(err)
	}
	if err := l.complete("r1", &pb.MonitorCommandResult{RequestId: "r1", Status: "success"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := l.begin(rollbackRequest("r2", "v3"))
	if !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("error = %v, want UNAVAILABLE", err)
	}
	if l.entries["r1"] == nil {
		t.Fatal("capacity rejection silently evicted existing entry")
	}
}

func TestRollbackLedgerRestartReconcilesInProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), rollbackLedgerFile)
	first := newRollbackLedger(path, 4, "rollback", "")
	if err := first.initialize(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.begin(rollbackRequest("r1", "v2")); err != nil {
		t.Fatal(err)
	}
	second := newRollbackLedger(path, 4, "rollback", "")
	if err := second.initialize(); err != nil {
		t.Fatal(err)
	}
	results, err := second.pendingResults()
	if err != nil || len(results) != 1 {
		t.Fatalf("pending = %+v, %v", results, err)
	}
	if results[0].GetErrorCode() != string(phelixerr.CodeUnavailable) || !strings.Contains(results[0].GetError(), "indeterminate") {
		t.Fatalf("reconciled result = %+v", results[0])
	}
	outcome, replay, err := second.begin(rollbackRequest("r1", "v2"))
	if err != nil || outcome != rollbackBeginReplay || replay.GetErrorCode() != string(phelixerr.CodeUnavailable) {
		t.Fatalf("restart replay = %v %+v %v", outcome, replay, err)
	}
}

func TestRollbackLedgerDeliveredStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), rollbackLedgerFile)
	first := newRollbackLedger(path, 4, "rollback", "")
	if err := first.initialize(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.begin(rollbackRequest("r1", "v2")); err != nil {
		t.Fatal(err)
	}
	if err := first.complete("r1", &pb.MonitorCommandResult{RequestId: "r1", Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := first.markDelivered("r1"); err != nil {
		t.Fatal(err)
	}
	second := newRollbackLedger(path, 4, "rollback", "")
	if err := second.initialize(); err != nil {
		t.Fatal(err)
	}
	pending, err := second.pendingResults()
	if err != nil || len(pending) != 0 {
		t.Fatalf("delivered result replayed after restart: %+v, %v", pending, err)
	}
}

func TestRollbackLedgerCorruptionFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), rollbackLedgerFile)
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newRollbackLedger(path, 4, "rollback", "")
	if err := l.initialize(); !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("initialize error = %v, want UNAVAILABLE", err)
	}
	if _, _, err := l.begin(rollbackRequest("r1", "v2")); !phelixerr.IsCode(err, phelixerr.CodeUnavailable) {
		t.Fatalf("begin error = %v, want fail-closed UNAVAILABLE", err)
	}
}

func TestRollbackLedgerFileMode0600(t *testing.T) {
	l := testRollbackLedger(t, 4)
	if _, _, err := l.begin(rollbackRequest("r1", "v2")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(l.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("ledger mode = %o, want 600", got)
	}
}
