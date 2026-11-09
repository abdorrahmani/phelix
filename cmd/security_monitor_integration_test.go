package cmd

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/config"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/security"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type securityMonitorBackend struct {
	pb.UnimplementedPhelixServiceServer
	controls chan *pb.MonitorControl
	events   chan *pb.MonitorEvent
	drop     chan struct{}
}

func (b *securityMonitorBackend) MonitorStream(stream pb.PhelixService_MonitorStreamServer) error {
	done := make(chan error, 1)
	go func() {
		for {
			event, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			select {
			case b.events <- event:
			case <-stream.Context().Done():
				return
			}
		}
	}()
	for {
		select {
		case err := <-done:
			return err
		case <-b.drop:
			return status.Error(codes.Unavailable, "test connection loss")
		case <-stream.Context().Done():
			return stream.Context().Err()
		case control := <-b.controls:
			if err := stream.Send(control); err != nil {
				return err
			}
		}
	}
}

func TestSecurityMonitorEndToEnd(t *testing.T) {
	if os.Getenv("PHELIX_SECURITY_MONITOR_TEST") != "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestSecurityMonitorEndToEnd$", "-test.timeout=45s")
		child.Env = append(os.Environ(), "PHELIX_SECURITY_MONITOR_TEST=1")
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("MonitorStream integration: %v\n%s", err, output)
		}
		return
	}
	dir := securityRemoteFixture(t, true)
	osv := securityOSV(t, "high")
	defer osv.Close()
	t.Setenv("PHELIX_OSV_URL", osv.URL)
	session, err := json.Marshal(map[string]any{"sessionID": "integration", "token": "integration", "scope": "agent", "expiresAt": time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(securityTestDataDir(), "agent-session.json"), session, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := &securityMonitorBackend{controls: make(chan *pb.MonitorControl, 16), events: make(chan *pb.MonitorEvent, 128), drop: make(chan struct{}, 1)}
	server := grpc.NewServer()
	pb.RegisterPhelixServiceServer(server, backend)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	if err := config.Load(); err != nil {
		t.Fatal(err)
	}
	previous := *config.Get()
	config.Get().App.Mode = "dev"
	config.Get().App.GRPCUrl = listener.Addr().String()
	defer func() { *config.Get() = previous }()
	if err := phelixgrpc.InitializeSecurityLedger(); err != nil {
		t.Fatal(err)
	}
	var runs, writes atomic.Int32
	blocked, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	phelixgrpc.SetSecurityHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		if req.Type == phelixgrpc.SecurityCommandRun {
			runs.Add(1)
		}
		if req.Type == phelixgrpc.SecurityCommandConfigSet {
			writes.Add(1)
		}
		result := RemoteSecurityCommand(req)
		if req.RequestId == "disconnect-scan" {
			close(blocked)
			<-release
		}
		return result
	})
	defer phelixgrpc.SetSecurityHandler(nil)
	client := phelixgrpc.NewClient()
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	client.StartMonitorStream()
	defer func() {
		releaseOnce.Do(func() { close(release) })
		phelixgrpc.WaitPendingSecurityCommands(3 * time.Second)
		client.Close()
	}()
	await := func(match func(*pb.MonitorEvent) bool) *pb.MonitorEvent {
		t.Helper()
		deadline := time.NewTimer(8 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case event := <-backend.events:
				if match(event) {
					return event
				}
			case <-deadline.C:
				t.Fatal("MonitorStream event timed out")
				return nil
			}
		}
	}
	first := await(func(ev *pb.MonitorEvent) bool { return ev.GetAgentStatus() != nil }).GetAgentStatus()
	if first.GetSecurityProtocolVersion() != 2 || first.GetSessionScope() != "agent" {
		t.Fatalf("status: %v", first)
	}
	request := func(id, command string, options *pb.SecurityCommandOptions) *pb.MonitorCommandResult {
		t.Helper()
		backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: securityRequest(id, command, securityRemoteAppID, options)}}
		result := await(func(ev *pb.MonitorEvent) bool { return ev.GetCommandResult().GetRequestId() == id }).GetCommandResult()
		if result.GetAppName() != securityRemoteAppID || result.GetCommand() != command {
			t.Fatalf("correlation: %v", result)
		}
		return result
	}
	if got := request("empty-report", phelixgrpc.SecurityCommandReport, nil); got.GetErrorCode() != "SCAN_NOT_FOUND" {
		t.Fatalf("empty report: %v", got)
	}
	if got := request("empty-history", phelixgrpc.SecurityCommandHistory, nil); got.GetSecurityScanList().GetTotal() != 0 || got.Status != "success" {
		t.Fatalf("empty history: %v", got)
	}
	backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: securityRequest("unknown-app", phelixgrpc.SecurityCommandHistory, "missing-app", nil)}}
	if got := await(func(ev *pb.MonitorEvent) bool { return ev.GetCommandResult().GetRequestId() == "unknown-app" }).GetCommandResult(); got.GetErrorCode() != "NOT_FOUND" {
		t.Fatalf("unknown app: %v", got)
	}
	run := request("run", phelixgrpc.SecurityCommandRun, nil)
	if run.Status != "success" || run.GetSecurityScanRecord().GetScanId() == "" {
		t.Fatalf("run: %v", run)
	}
	if got := request("run", phelixgrpc.SecurityCommandRun, nil); got.GetSecurityScanRecord().GetScanId() != run.GetSecurityScanRecord().GetScanId() || runs.Load() != 1 {
		t.Fatalf("duplicate scan: %v", got)
	}
	report := request("report", phelixgrpc.SecurityCommandReport, &pb.SecurityCommandOptions{IncludeGraph: true})
	if report.GetSecurityScanRecord().GetGraph().GetEcosystem() != "go" || len(report.GetSecurityScanRecord().GetFindings()) == 0 {
		t.Fatalf("report/graph: %v", report)
	}
	detail := request("detail", phelixgrpc.SecurityCommandDetail, &pb.SecurityCommandOptions{ScanId: run.GetSecurityScanRecord().GetScanId(), IncludeGraph: true})
	if len(detail.GetSecurityScanRecord().GetGraph().GetNodes()) == 0 {
		t.Fatalf("detail: %v", detail)
	}
	if got := request("advisory", phelixgrpc.SecurityCommandAdvisory, &pb.SecurityCommandOptions{AdvisoryId: "GO-2024-3001"}); got.GetSecurityAdvisory().GetId() != "GO-2024-3001" {
		t.Fatalf("advisory: %v", got)
	}
	options := &pb.SecurityCommandOptions{SetEnabled: true, Enabled: false}
	for range 2 {
		if got := request("config-set", phelixgrpc.SecurityCommandConfigSet, options); got.Status != "success" || got.GetSecurityConfig().GetEnabled() {
			t.Fatalf("disable: %v", got)
		}
	}
	if writes.Load() != 1 {
		t.Fatal("duplicate configuration mutation")
	}
	if got := request("config-set", phelixgrpc.SecurityCommandConfigSet, &pb.SecurityCommandOptions{SetEnabled: true, Enabled: true}); got.GetErrorCode() != "ALREADY_EXISTS" {
		t.Fatalf("payload mismatch: %v", got)
	}
	if got := request("version", phelixgrpc.SecurityCommandConfig, &pb.SecurityCommandOptions{ProtocolVersion: 3}); got.GetErrorCode() != "UNIMPLEMENTED" {
		t.Fatalf("version mismatch: %v", got)
	}
	backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: securityRequest("disconnect-scan", phelixgrpc.SecurityCommandRun, securityRemoteAppID, nil)}}
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not reach completion")
	}
	backend.drop <- struct{}{}
	releaseOnce.Do(func() { close(release) })
	second := await(func(ev *pb.MonitorEvent) bool {
		return ev.GetAgentStatus() != nil && ev.GetAgentStatus().GetConnectionId() != first.GetConnectionId()
	}).GetAgentStatus()
	if second.GetConnectionId() == "" {
		t.Fatal("reconnect did not advertise identity")
	}
	replayed := await(func(ev *pb.MonitorEvent) bool { return ev.GetCommandResult().GetRequestId() == "disconnect-scan" }).GetCommandResult()
	if replayed.Status != "success" || runs.Load() != 2 {
		t.Fatalf("recovery: %v", replayed)
	}
	backend.controls <- &pb.MonitorControl{Payload: &pb.MonitorControl_SecurityResultAck{SecurityResultAck: &pb.SecurityResultAck{RequestId: replayed.RequestId, Command: replayed.Command, AppName: replayed.AppName}}}
	history := request("history-page", phelixgrpc.SecurityCommandHistory, &pb.SecurityCommandOptions{Limit: 1})
	if !history.GetSecurityScanList().GetHasMore() || len(history.GetSecurityScanList().GetScans()) != 1 {
		t.Fatalf("history: %v", history)
	}
	if got := request("history-next", phelixgrpc.SecurityCommandHistory, &pb.SecurityCommandOptions{Limit: 1, Offset: 1}); got.GetSecurityScanList().GetHasMore() {
		t.Fatalf("history page 2: %v", got)
	}
	for _, tc := range []struct{ yaml, code string }{
		{"name: remoteapp\nsecurity:\n  vulnerability:\n    reporting: false\n", "SECURITY_REPORTING_DISABLED"},
		{"security: [", "SECURITY_CONFIG_INVALID"},
	} {
		if err := os.WriteFile(filepath.Join(dir, "phelix.yaml"), []byte(tc.yaml), 0600); err != nil {
			t.Fatal(err)
		}
		if got := request(tc.code, phelixgrpc.SecurityCommandReport, nil); got.GetErrorCode() != tc.code || got.GetSecurityScanRecord() != nil {
			t.Fatalf("privacy/config: %v", got)
		}
		if tc.code == "SECURITY_REPORTING_DISABLED" {
			if got := request("consent-status", phelixgrpc.SecurityCommandConfig, nil); got.Status != "success" || got.GetSecurityConfig().GetReporting() != "disabled" || got.GetSecurityConfig().GetLastScanId() != "" {
				t.Fatalf("safe status: %v", got)
			}
			if got := request("run", phelixgrpc.SecurityCommandRun, nil); !got.GetSecurityResultWithheld() || got.GetSecurityScanRecord() != nil || runs.Load() != 2 {
				t.Fatalf("replay privacy: %v", got)
			}
		}
	}
	stored, err := security.NewScanStore(securityTestDataDir()).ListScans(securityRemoteAppID)
	if err != nil || len(stored) != 2 {
		t.Fatalf("scan count = %d: %v", len(stored), err)
	}
}
