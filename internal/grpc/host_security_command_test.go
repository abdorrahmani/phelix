package grpc

import (
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/hostsecurity"
	"github.com/abdorrahmani/phelix/internal/server"
)

func hostSecurityTestRequest(id, command string, opts *pb.HostSecurityCommandOptions) *pb.MonitorCommandRequest {
	return &pb.MonitorCommandRequest{
		RequestId:    id,
		Type:         command,
		HostSecurity: opts,
	}
}

// TestValidateHostSecurityCommand covers the transport shape: correlation,
// foreign options from every other command family, and per-type option
// rules.
func TestValidateHostSecurityCommand(t *testing.T) {
	cases := []struct {
		name    string
		req     *pb.MonitorCommandRequest
		wantErr bool
	}{
		{
			name: "scan run without options",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandScanRun, nil),
		},
		{
			name:    "app_name is a foreign field",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: HostSecurityCommandScanRun, AppName: "app"},
			wantErr: true,
		},
		{
			name:    "security options are a foreign family",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: HostSecurityCommandScanRun, Security: &pb.SecurityCommandOptions{}},
			wantErr: true,
		},
		{
			name:    "job options are a foreign family",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: HostSecurityCommandScanRun, Job: &pb.JobOptions{}},
			wantErr: true,
		},
		{
			name:    "hook options are a foreign family",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: HostSecurityCommandScanRun, Hooks: &pb.HookOptions{}},
			wantErr: true,
		},
		{
			name:    "rollback target is a foreign field",
			req:     &pb.MonitorCommandRequest{RequestId: "r1", Type: HostSecurityCommandScanRun, Target: "v3"},
			wantErr: true,
		},
		{
			name:    "missing request id",
			req:     &pb.MonitorCommandRequest{Type: HostSecurityCommandScanRun},
			wantErr: true,
		},
		{
			name:    "unknown command type",
			req:     hostSecurityTestRequest("r1", "host_security_nonsense", nil),
			wantErr: true,
		},
		{
			name:    "unsupported protocol version",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandReport, &pb.HostSecurityCommandOptions{ProtocolVersion: HostSecurityProtocolVersion + 1}),
			wantErr: true,
		},
		{
			name: "report with filters",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandReport, &pb.HostSecurityCommandOptions{Severity: "high", State: "confirmed", Package: "ssl", FindingsOffset: 10}),
		},
		{
			name:    "report with paging",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandReport, &pb.HostSecurityCommandOptions{Offset: 5}),
			wantErr: true,
		},
		{
			name:    "report with invalid severity",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandReport, &pb.HostSecurityCommandOptions{Severity: "catastrophic"}),
			wantErr: true,
		},
		{
			name: "history with paging",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandHistory, &pb.HostSecurityCommandOptions{Limit: 50, Offset: 10}),
		},
		{
			name:    "history over the page cap",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandHistory, &pb.HostSecurityCommandOptions{Limit: 101}),
			wantErr: true,
		},
		{
			name:    "history with filters",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandHistory, &pb.HostSecurityCommandOptions{Severity: "high"}),
			wantErr: true,
		},
		{
			name:    "detail without scan id",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandDetail, nil),
			wantErr: true,
		},
		{
			name: "detail with scan id and filters",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: "scn", Severity: "high", FindingsOffset: 5}),
		},
		{
			name:    "detail with paging",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: "scn", Limit: 20}),
			wantErr: true,
		},
		{
			name: "inventory with filter and paging",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandInventory, &pb.HostSecurityCommandOptions{ScanId: "scn", Package: "ssl", Limit: 10, Offset: 5}),
		},
		{
			name:    "inventory with severity filter",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandInventory, &pb.HostSecurityCommandOptions{Severity: "high"}),
			wantErr: true,
		},
		{
			name:    "update preview without packages",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandUpdatePreview, &pb.HostSecurityCommandOptions{}),
			wantErr: true,
		},
		{
			name:    "update preview with a bad package spec",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandUpdatePreview, &pb.HostSecurityCommandOptions{Packages: []string{"-openssl"}}),
			wantErr: true,
		},
		{
			name:    "update preview with a token",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandUpdatePreview, &pb.HostSecurityCommandOptions{Packages: []string{"openssl"}, PreviewToken: "tok"}),
			wantErr: true,
		},
		{
			name: "update preview with packages",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandUpdatePreview, &pb.HostSecurityCommandOptions{Packages: []string{"openssl", "libssl3=3.0.12"}}),
		},
		{
			name:    "update apply without token",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandUpdateApply, &pb.HostSecurityCommandOptions{Packages: []string{"openssl"}}),
			wantErr: true,
		},
		{
			name:    "update apply without packages",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandUpdateApply, &pb.HostSecurityCommandOptions{PreviewToken: "tok"}),
			wantErr: true,
		},
		{
			name: "update apply with packages and token",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandUpdateApply, &pb.HostSecurityCommandOptions{Packages: []string{"openssl"}, PreviewToken: "tok"}),
		},
		{
			name:    "update verify without operation id",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandUpdateVerify, nil),
			wantErr: true,
		},
		{
			name: "update verify with operation id",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandUpdateVerify, &pb.HostSecurityCommandOptions{OperationId: "hso_1"}),
		},
		{
			name: "reboot status without options",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandRebootStatus, nil),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHostSecurityCommand(tc.req)
			if tc.wantErr && err == nil {
				t.Fatal("expected a validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// capturingHostSecurityStream records the events the result sender pushes.
type capturingHostSecurityStream struct {
	pb.PhelixService_MonitorStreamClient
	events []*pb.MonitorEvent
}

func (s *capturingHostSecurityStream) Send(event *pb.MonitorEvent) error {
	s.events = append(s.events, event)
	return nil
}

// TestSendHostSecurityResult_WithholdsWithoutConsent pins the withholding
// discipline: without reporting consent the host security payload is
// stripped and the withholding markers tell the backend the answer was
// suppressed; queries report the consent error, mutations keep their
// terminal status. The config answer is never withheld.
func TestSendHostSecurityResult_WithholdsWithoutConsent(t *testing.T) {
	setupTestSession(t)

	prev := monitorStream.stream
	stream := &capturingHostSecurityStream{}
	monitorStream.stream = stream
	t.Cleanup(func() { monitorStream.stream = prev })

	record := &pb.HostSecurityScanRecord{ScanId: "scan-1"}
	withConsent := func(t *testing.T) {
		t.Helper()
		if err := hostsecurity.SaveHostConfig(server.DataDir(), hostsecurity.HostConfig{Reporting: true, ScanInterval: "24h"}); err != nil {
			t.Fatal(err)
		}
	}
	withConsent(t)

	query := &pb.MonitorCommandResult{RequestId: "q1", Command: HostSecurityCommandReport, Status: "success", HostSecurityScanRecord: record}
	if err := sendHostSecurityResult(query); err != nil {
		t.Fatalf("send with consent: %v", err)
	}
	if got := stream.events[len(stream.events)-1].GetCommandResult().GetHostSecurityScanRecord(); got == nil {
		t.Fatalf("consented result must carry the payload: %+v", stream.events[len(stream.events)-1].GetCommandResult())
	}

	if err := hostsecurity.SaveHostConfig(server.DataDir(), hostsecurity.HostConfig{Reporting: false, ScanInterval: "24h"}); err != nil {
		t.Fatal(err)
	}

	query = &pb.MonitorCommandResult{RequestId: "q2", Command: HostSecurityCommandReport, Status: "success", HostSecurityScanRecord: record}
	if err := sendHostSecurityResult(query); err != nil {
		t.Fatalf("send without consent: %v", err)
	}
	result := stream.events[len(stream.events)-1].GetCommandResult()
	if result.GetHostSecurityScanRecord() != nil || !result.GetSecurityResultWithheld() ||
		result.GetSecurityConsentErrorCode() != "HOST_SECURITY_CONSENT_DISABLED" ||
		result.GetStatus() != "error" || result.GetErrorCode() != "HOST_SECURITY_CONSENT_DISABLED" {
		t.Fatalf("query must be withheld with the consent error: %+v", result)
	}

	mutation := &pb.MonitorCommandResult{RequestId: "m1", Command: HostSecurityCommandScanRun, Status: "success", HostSecurityScanRecord: record}
	if err := sendHostSecurityResult(mutation); err != nil {
		t.Fatalf("send mutation without consent: %v", err)
	}
	result = stream.events[len(stream.events)-1].GetCommandResult()
	if result.GetHostSecurityScanRecord() != nil || !result.GetSecurityResultWithheld() ||
		result.GetStatus() != "success" || result.GetErrorCode() != "" {
		t.Fatalf("mutation must be withheld while keeping its status: %+v", result)
	}

	config := &pb.MonitorCommandResult{RequestId: "c1", Command: HostSecurityCommandConfig, Status: "success", HostSecurityConfig: &pb.HostSecurityConfigState{ReportingEnabled: false}}
	if err := sendHostSecurityResult(config); err != nil {
		t.Fatalf("send config without consent: %v", err)
	}
	result = stream.events[len(stream.events)-1].GetCommandResult()
	if result.GetHostSecurityConfig() == nil || result.GetSecurityResultWithheld() {
		t.Fatalf("the config answer must never be withheld: %+v", result)
	}
}

// TestHostSecurityAckMarksDelivered pins the acknowledgement contract: the
// backend's SecurityResultAck marks the ledger entry delivered so the
// replay loop stops re-sending it.
func TestHostSecurityAckMarksDelivered(t *testing.T) {
	path := t.TempDir() + "/host-security-ledger.json"
	ledger := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "indeterminate")
	if err := ledger.initializeAt(path); err != nil {
		t.Fatal(err)
	}
	req := hostSecurityTestRequest("req-1", HostSecurityCommandScanRun, nil)
	outcome, _, err := ledger.begin(req)
	if err != nil || outcome != rollbackBeginExecute {
		t.Fatalf("begin: %v %v", outcome, err)
	}
	result := &pb.MonitorCommandResult{Status: "success", HostSecurityScanRecord: &pb.HostSecurityScanRecord{ScanId: "scan-1"}}
	finalized := finalizeHostSecurityResult(req, result)
	if err := ledger.complete("req-1", finalized); err != nil {
		t.Fatalf("complete: %v", err)
	}

	acknowledgeHostSecurityResult(ledger, &pb.SecurityResultAck{RequestId: "req-1", Command: HostSecurityCommandScanRun})
	ledger.mu.Lock()
	delivered := ledger.entries["req-1"].Delivered
	ledger.mu.Unlock()
	if !delivered {
		t.Fatal("the acknowledgement must mark the entry delivered")
	}

	acknowledgeHostSecurityResult(ledger, &pb.SecurityResultAck{RequestId: "req-1", Command: HostSecurityCommandUpdateApply})
	ledger.mu.Lock()
	stillComplete := ledger.entries["req-1"].State == ledgerStateComplete
	ledger.mu.Unlock()
	if !stillComplete {
		t.Fatal("a mismatched acknowledgement must not corrupt the entry")
	}
}

// TestHostSecurityLedgerPaths pins the per-family ledger separation: the
// host security ledger lives in its own file, not the other families'.
func TestHostSecurityLedgerPaths(t *testing.T) {
	if HostSecurityLedgerPath() == SecurityLedgerPath() {
		t.Fatal("host security and application security ledgers must be separate files")
	}
	if time.Duration(0) >= HostSecurityCommandSettleTimeout() {
		t.Fatal("the settle timeout must be positive")
	}
}
