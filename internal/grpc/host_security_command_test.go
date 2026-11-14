package grpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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
			name: "report with the findings page size",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandReport, &pb.HostSecurityCommandOptions{Severity: "high", FindingsOffset: 10, Limit: 25}),
		},
		{
			name:    "report with a page offset",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandReport, &pb.HostSecurityCommandOptions{Offset: 5}),
			wantErr: true,
		},
		{
			name:    "report over the page cap",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandReport, &pb.HostSecurityCommandOptions{Limit: 101}),
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
			name: "detail with the findings page size",
			req:  hostSecurityTestRequest("r1", HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: "scn", FindingsOffset: 25, Limit: 25}),
		},
		{
			name:    "detail with a page offset",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: "scn", Offset: 5}),
			wantErr: true,
		},
		{
			name:    "detail with a negative page size",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: "scn", Limit: -1}),
			wantErr: true,
		},
		{
			name:    "detail over the page cap",
			req:     hostSecurityTestRequest("r1", HostSecurityCommandDetail, &pb.HostSecurityCommandOptions{ScanId: "scn", Limit: 101}),
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
// discipline: without reporting consent the host security query answer is
// delivered with its payload stripped and the withholding markers carry the
// consent error, while a mutation result is never delivered in stripped
// form. The config answer is never withheld.
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
	for _, event := range stream.events {
		if event.GetCommandResult().GetRequestId() == "m1" {
			t.Fatalf("a withheld mutation result must not be delivered in stripped form: %+v", event.GetCommandResult())
		}
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

// TestHostSecurityWithheldReplayKeepsEvidence pins the no-store discipline
// for consent-withheld mutation results: consent flipped off between execute
// and send delivers nothing, the durable ledger result keeps its payload
// through every withheld replay, and restored consent replays the full
// result.
func TestHostSecurityWithheldReplayKeepsEvidence(t *testing.T) {
	setupTestSession(t)

	prev := monitorStream.stream
	stream := &capturingHostSecurityStream{}
	monitorStream.stream = stream
	t.Cleanup(func() { monitorStream.stream = prev })

	path := t.TempDir() + "/host-security-ledger.json"
	ledger := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "indeterminate")
	if err := ledger.initializeAt(path); err != nil {
		t.Fatal(err)
	}
	req := hostSecurityTestRequest("req-evidence", HostSecurityCommandScanRun, nil)
	if err := hostsecurity.SaveHostConfig(server.DataDir(), hostsecurity.HostConfig{Reporting: true, ScanInterval: "24h"}); err != nil {
		t.Fatal(err)
	}
	outcome, _, err := ledger.begin(req)
	if err != nil || outcome != rollbackBeginExecute {
		t.Fatalf("begin: %v %v", outcome, err)
	}
	result := finalizeHostSecurityResult(req, &pb.MonitorCommandResult{Status: "success", HostSecurityScanRecord: &pb.HostSecurityScanRecord{ScanId: "scan-evidence"}})
	if err := ledger.complete(req.GetRequestId(), result); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if err := hostsecurity.SaveHostConfig(server.DataDir(), hostsecurity.HostConfig{Reporting: false, ScanInterval: "24h"}); err != nil {
		t.Fatal(err)
	}
	if err := sendHostSecurityResult(result); err != nil {
		t.Fatalf("withheld send: %v", err)
	}
	(&Client{}).replayPendingHostSecurityResults(ledger)
	delivered := 0
	for _, event := range stream.events {
		if event.GetCommandResult().GetRequestId() == "req-evidence" {
			delivered++
		}
	}
	if delivered != 0 {
		t.Fatal("a consent-withheld mutation result must not be delivered while consent is off")
	}
	pending, err := ledger.pendingResults()
	if err != nil || len(pending) != 1 || pending[0].GetHostSecurityScanRecord().GetScanId() != "scan-evidence" {
		t.Fatalf("the durable result must keep its payload: %+v %v", pending, err)
	}

	if err := hostsecurity.SaveHostConfig(server.DataDir(), hostsecurity.HostConfig{Reporting: true, ScanInterval: "24h"}); err != nil {
		t.Fatal(err)
	}
	(&Client{}).replayPendingHostSecurityResults(ledger)
	replayed := 0
	for _, event := range stream.events {
		got := event.GetCommandResult()
		if got.GetRequestId() == "req-evidence" {
			replayed++
			if got.GetHostSecurityScanRecord().GetScanId() != "scan-evidence" || got.GetStatus() != "success" || got.GetSecurityResultWithheld() {
				t.Fatalf("restored consent must replay the full result: %+v", got)
			}
		}
	}
	if replayed != 1 {
		t.Fatalf("restored consent must replay exactly once, got %d", replayed)
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

// TestHostSecurityIndeterminateAckFreesSlot pins the recovery contract for
// an agent restart during a mutation: the in-progress entry becomes a
// complete undelivered indeterminate result that replays once, the backend
// records that outcome and acknowledges it, and the acknowledgement frees
// the ledger slot so the entry can never wedge the window. Acks for
// in-progress entries are ignored safely, so the replay of a recorded
// result can never be corrupted by a stale ack.
func TestHostSecurityIndeterminateAckFreesSlot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-security-ledger.json")
	ledger := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "reconcile with a host_security_config or host_security_scan_detail command")
	if err := ledger.initialize(); err != nil {
		t.Fatal(err)
	}
	req := hostSecurityTestRequest("req-indeterminate", HostSecurityCommandScanRun, nil)
	if _, _, err := ledger.begin(req); err != nil {
		t.Fatal(err)
	}

	// The agent restarts: a fresh ledger over the same file converts the
	// in-progress entry into a complete undelivered indeterminate result.
	restarted := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "reconcile with a host_security_config or host_security_scan_detail command")
	if err := restarted.initialize(); err != nil {
		t.Fatal(err)
	}
	pending, err := restarted.pendingResults()
	if err != nil || len(pending) != 1 || pending[0].GetRequestId() != req.GetRequestId() {
		t.Fatalf("the indeterminate result must replay after the restart: %+v %v", pending, err)
	}
	if pending[0].GetErrorCode() != string(phelixerr.CodeUnavailable) || pending[0].GetCommand() != HostSecurityCommandScanRun {
		t.Fatalf("the restart answer must be an UNAVAILABLE result for the original command: %+v", pending[0])
	}
	outcome, replay, err := restarted.begin(req)
	if err != nil || outcome != rollbackBeginReplay || replay.GetErrorCode() != string(phelixerr.CodeUnavailable) {
		t.Fatalf("a retry must replay the indeterminate answer without re-executing: %v %v %v", outcome, replay, err)
	}

	// markDelivered refuses an entry that is not complete, so a stale ack
	// racing a live execution can never corrupt the in-progress record.
	if err := ledger.markDelivered(req.GetRequestId()); err == nil {
		t.Fatal("markDelivered must refuse an entry that is not complete")
	}
	pending, err = restarted.pendingResults()
	if err != nil || len(pending) != 1 {
		t.Fatalf("the refused ack must leave the indeterminate result pending: %+v %v", pending, err)
	}

	// The backend records the indeterminate outcome and acknowledges it.
	acknowledgeHostSecurityResult(restarted, &pb.SecurityResultAck{RequestId: req.GetRequestId(), Command: HostSecurityCommandScanRun})
	pending, err = restarted.pendingResults()
	if err != nil || len(pending) != 0 {
		t.Fatalf("the acknowledgement must free the ledger slot: %+v %v", pending, err)
	}

	// The freed slot stays a durable dedup identity: no replay after another
	// restart, and the request still replays its recorded answer.
	afterAck := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "")
	if err := afterAck.initialize(); err != nil {
		t.Fatal(err)
	}
	pending, err = afterAck.pendingResults()
	if err != nil || len(pending) != 0 {
		t.Fatalf("the ack must survive a restart: %+v %v", pending, err)
	}
	outcome, replay, err = afterAck.begin(req)
	if err != nil || outcome != rollbackBeginReplay || replay.GetErrorCode() != string(phelixerr.CodeUnavailable) {
		t.Fatalf("the ack must keep the dedup identity: %v %v %v", outcome, replay, err)
	}

	// The freed entry stays evictable: a full window evicts it instead of
	// wedging future mutations.
	acknowledged := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "")
	if err := acknowledged.initialize(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rollbackLedgerCapacity; i++ {
		fill := hostSecurityTestRequest(fmt.Sprintf("fill-%04d", i), HostSecurityCommandScanRun, nil)
		outcome, _, err := acknowledged.begin(fill)
		if err != nil || outcome != rollbackBeginExecute {
			t.Fatalf("the window must admit new mutations after the ack: %d %v %v", i, outcome, err)
		}
	}
}

// TestHostSecurityIndeterminateTTLValve pins the safety valve: an ancient
// undelivered indeterminate entry is dropped from the ledger with the
// recent indeterminate entries, real recorded outcomes, and delivered
// entries untouched, so a backend that never acknowledges cannot wedge the
// agent permanently.
func TestHostSecurityIndeterminateTTLValve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-security-ledger.json")
	seed := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "")
	if err := seed.initialize(); err != nil {
		t.Fatal(err)
	}
	ancient := hostSecurityTestRequest("req-ancient", HostSecurityCommandScanRun, nil)
	recent := hostSecurityTestRequest("req-recent", HostSecurityCommandScanRun, nil)
	outcome := hostSecurityTestRequest("req-outcome", HostSecurityCommandScanRun, nil)
	acked := hostSecurityTestRequest("req-acked", HostSecurityCommandScanRun, nil)
	for _, req := range []*pb.MonitorCommandRequest{ancient, recent, outcome, acked} {
		if o, _, err := seed.begin(req); err != nil || o != rollbackBeginExecute {
			t.Fatalf("begin %s: %v %v", req.GetRequestId(), o, err)
		}
	}
	result := finalizeHostSecurityResult(outcome, &pb.MonitorCommandResult{Status: "success"})
	if err := seed.complete(outcome.GetRequestId(), result); err != nil {
		t.Fatal(err)
	}
	if err := seed.complete(acked.GetRequestId(), finalizeHostSecurityResult(acked, &pb.MonitorCommandResult{Status: "success"})); err != nil {
		t.Fatal(err)
	}
	if err := seed.markDelivered(acked.GetRequestId()); err != nil {
		t.Fatal(err)
	}

	// The restart converts the two in-progress entries into indeterminate
	// results, then the file is aged: only the ancient indeterminate entry
	// crosses the valve.
	restarted := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "")
	if err := restarted.initialize(); err != nil {
		t.Fatal(err)
	}
	ancientAt := time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	if err := ageLedgerEntry(path, ancient.GetRequestId(), ancientAt); err != nil {
		t.Fatal(err)
	}
	if err := ageLedgerEntry(path, outcome.GetRequestId(), ancientAt); err != nil {
		t.Fatal(err)
	}
	if err := ageLedgerEntry(path, acked.GetRequestId(), ancientAt); err != nil {
		t.Fatal(err)
	}

	valve := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "")
	if err := valve.initialize(); err != nil {
		t.Fatal(err)
	}
	valve.mu.Lock()
	_, ancientGone := valve.entries[ancient.GetRequestId()]
	_, recentKept := valve.entries[recent.GetRequestId()]
	_, outcomeKept := valve.entries[outcome.GetRequestId()]
	_, ackedKept := valve.entries[acked.GetRequestId()]
	pendingAncient := valve.pending[ancient.GetRequestId()]
	valve.mu.Unlock()
	if ancientGone {
		t.Fatal("the ancient undelivered indeterminate entry must be dropped")
	}
	if !recentKept || !pendingRecent(valve, recent.GetRequestId()) {
		t.Fatal("a recent indeterminate entry must stay pending")
	}
	if !outcomeKept {
		t.Fatal("a recorded real outcome must never be aged out")
	}
	if !ackedKept {
		t.Fatal("a delivered entry must never be aged out")
	}
	if pendingAncient {
		t.Fatal("the dropped entry must leave the replay queue")
	}

	// The drop is durable: another restart does not resurrect it.
	again := newRollbackLedger(path, rollbackLedgerCapacity, "host_security", "")
	if err := again.initialize(); err != nil {
		t.Fatal(err)
	}
	again.mu.Lock()
	_, resurrected := again.entries[ancient.GetRequestId()]
	again.mu.Unlock()
	if resurrected {
		t.Fatal("the valve drop must survive a restart")
	}
}

func pendingRecent(ledger *rollbackLedger, requestID string) bool {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return ledger.pending[requestID]
}

// ageLedgerEntry rewrites one entry's updated_at timestamp in the ledger
// file so tests can age it past the valve.
func ageLedgerEntry(path, requestID string, updatedAt int64) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var disk rollbackLedgerFileData
	if err := json.Unmarshal(raw, &disk); err != nil {
		return err
	}
	found := false
	for _, entry := range disk.Entries {
		if entry != nil && entry.RequestID == requestID {
			entry.UpdatedAt = updatedAt
			found = true
		}
	}
	if !found {
		return errors.New("entry not found in ledger file: " + requestID)
	}
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
