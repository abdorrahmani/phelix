package grpc

import (
	"path/filepath"
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func securityTestRequest(id string, app string, opts *pb.SecurityCommandOptions) *pb.MonitorCommandRequest {
	return &pb.MonitorCommandRequest{
		RequestId: id,
		Type:      SecurityCommandRun,
		AppName:   app,
		Security:  opts,
	}
}

// TestValidateSecurityCommand covers the transport shape: correlation,
// foreign options, and per-type option rules.
func TestValidateSecurityCommand(t *testing.T) {
	valid := &pb.SecurityCommandOptions{ScanId: "scn_0000000000000001"}
	cases := []struct {
		name    string
		req     *pb.MonitorCommandRequest
		wantErr bool
	}{
		{
			name: "scan run without options",
			req:  &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandRun, AppName: "app"},
		},
		{
			name:    "scan run with filters",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandRun, AppName: "app", Security: &pb.SecurityCommandOptions{Severity: "high"}},
			wantErr: true,
		},
		{
			name:    "missing request id",
			req:     &pb.MonitorCommandRequest{Type: SecurityCommandRun, AppName: "app"},
			wantErr: true,
		},
		{
			name:    "missing app name",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandRun},
			wantErr: true,
		},
		{
			name:    "foreign webhook options",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandRun, AppName: "app", Webhook: &pb.WebhookOptions{}},
			wantErr: true,
		},
		{
			name:    "detail without scan id",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandDetail, AppName: "app"},
			wantErr: true,
		},
		{
			name: "detail with scan id and graph",
			req:  &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandDetail, AppName: "app", Security: valid},
		},
		{
			name:    "advisory without id",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandAdvisory, AppName: "app"},
			wantErr: true,
		},
		{
			name: "advisory with id",
			req:  &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandAdvisory, AppName: "app", Security: &pb.SecurityCommandOptions{AdvisoryId: "GHSA-1111-2222-3333"}},
		},
		{
			name:    "history over the page cap",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandHistory, AppName: "app", Security: &pb.SecurityCommandOptions{Limit: 101}},
			wantErr: true,
		},
		{
			name: "config set with fields",
			req:  &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandConfigSet, AppName: "app", Security: &pb.SecurityCommandOptions{SetEnabled: true, Enabled: true}},
		},
		{
			name:    "config set without fields",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: SecurityCommandConfigSet, AppName: "app", Security: &pb.SecurityCommandOptions{}},
			wantErr: true,
		},
		{
			name:    "unknown command type",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: "security_nonsense", AppName: "app"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSecurityCommand(tc.req)
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestSecurityLedger_Idempotency pins the persistent mutation contract:
// the same request id with the same payload replays the stored result, a
// reused id with a different payload is rejected, and the ledger survives
// a reload (process restart).
func TestSecurityLedger_Idempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "security-ledger.json")
	ledger := newRollbackLedger(path, rollbackLedgerCapacity, "security", "indeterminate")
	if err := ledger.initializeAt(path); err != nil {
		t.Fatalf("initializeAt: %v", err)
	}

	first := securityTestRequest("req-1", "app", nil)
	outcome, replay, err := ledger.begin(first)
	if err != nil || outcome != rollbackBeginExecute {
		t.Fatalf("first begin must execute: outcome=%v err=%v", outcome, err)
	}
	result := &pb.MonitorCommandResult{Status: "success", SecurityScanRecord: &pb.SecurityScanRecord{ScanId: "scn_0000000000000001"}}
	if err := ledger.complete("req-1", result); err != nil {
		t.Fatalf("complete: %v", err)
	}

	sameAgain := securityTestRequest("req-1", "app", nil)
	outcome, replay, err = ledger.begin(sameAgain)
	if err != nil || outcome != rollbackBeginReplay {
		t.Fatalf("same id and payload must replay: outcome=%v err=%v", outcome, err)
	}
	if replay.GetSecurityScanRecord().GetScanId() != "scn_0000000000000001" {
		t.Fatalf("the stored result replays byte-faithfully: %+v", replay)
	}

	conflict := securityTestRequest("req-1", "other-app", nil)
	if _, _, err := ledger.begin(conflict); err == nil {
		t.Fatal("a reused id with a different payload must be rejected")
	}

	reloaded := newRollbackLedger(path, rollbackLedgerCapacity, "security", "indeterminate")
	if err := reloaded.initializeAt(path); err != nil {
		t.Fatalf("reload: %v", err)
	}
	outcome, replay, err = reloaded.begin(sameAgain)
	if err != nil || outcome != rollbackBeginReplay {
		t.Fatalf("idempotency must survive a restart: outcome=%v err=%v", outcome, err)
	}
	if replay.GetSecurityScanRecord().GetScanId() != "scn_0000000000000001" {
		t.Fatalf("replay after restart lost the result: %+v", replay)
	}
}

// TestSecurityLedger_InProgressAnswersUnavailable pins concurrent
// duplicate handling: a second request with the same id while the first is
// in flight does not start duplicate work.
func TestSecurityLedger_InProgressAnswersUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "security-ledger.json")
	ledger := newRollbackLedger(path, rollbackLedgerCapacity, "security", "indeterminate")
	if err := ledger.initializeAt(path); err != nil {
		t.Fatal(err)
	}
	req := securityTestRequest("req-2", "app", nil)
	outcome, _, err := ledger.begin(req)
	if err != nil || outcome != rollbackBeginExecute {
		t.Fatalf("first begin: %v %v", outcome, err)
	}
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		outcome, _, err = ledger.begin(req)
		if err != nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("an in-progress request must not begin again")
}
