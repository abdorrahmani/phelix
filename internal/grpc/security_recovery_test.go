package grpc

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func TestSecurityLedgerConcurrentRestartAndAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	ledger := newRollbackLedger(path, 0, "security", "")
	if err := ledger.initialize(); err != nil {
		t.Fatal(err)
	}
	req := securityTestRequest("recover", "stable-app-id", nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	executions := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcome, _, err := ledger.begin(req)
			if err == nil && outcome == rollbackBeginExecute {
				mu.Lock()
				executions++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if executions != 1 {
		t.Fatalf("executions = %d", executions)
	}
	restarted := newRollbackLedger(path, 0, "security", "")
	if err := restarted.initialize(); err != nil {
		t.Fatal(err)
	}
	_, result, err := restarted.begin(req)
	if err != nil || result.GetErrorCode() != "SECURITY_OUTCOME_UNKNOWN" || result.GetAppName() != req.AppName {
		t.Fatalf("restart result: %v %v", result, err)
	}
	acknowledgeSecurityResult(restarted, &pb.SecurityResultAck{RequestId: req.RequestId, Command: req.Type, AppName: "wrong-app"})
	pending, _ := restarted.pendingResults()
	if len(pending) != 1 {
		t.Fatal("wrong identity acknowledged result")
	}
	acknowledgeSecurityResult(restarted, &pb.SecurityResultAck{RequestId: req.RequestId, Command: req.Type, AppName: req.AppName})
	pending, _ = restarted.pendingResults()
	if len(pending) != 0 {
		t.Fatal("acknowledged result remains pending")
	}
	_, replay, err := restarted.begin(req)
	if err != nil || replay.GetErrorCode() != "SECURITY_OUTCOME_UNKNOWN" {
		t.Fatal("ack removed durable identity")
	}
}

func TestSecurityQueryDoesNotBlockMonitorPing(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)
	previous := getSecurityHandler()
	entered, release := make(chan struct{}), make(chan struct{})
	SetSecurityHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		close(entered)
		<-release
		return &pb.MonitorCommandResult{Status: "error", ErrorCode: "SCAN_NOT_FOUND"}
	})
	defer SetSecurityHandler(previous)
	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()
	done := make(chan error, 1)
	go func() { done <- c.runMonitorStream() }()
	defer func() { c.Close(); <-done }()
	backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Command{Command: &pb.MonitorCommandRequest{RequestId: "slow", Type: SecurityCommandReport, AppName: "app"}}}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("query never started")
	}
	backend.toClient <- &pb.MonitorControl{Payload: &pb.MonitorControl_Ping{Ping: &pb.Ping{Id: "during-query"}}}
	pong := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool { return ev.GetPong().GetId() == "during-query" }, time.Second)
	close(release)
	WaitPendingSecurityCommands(time.Second)
	if pong == nil {
		t.Fatal("query blocked stream receive loop")
	}
	result := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool { return ev.GetCommandResult().GetRequestId() == "slow" }, time.Second)
	if result == nil || result.GetCommandResult().GetErrorCode() != "SCAN_NOT_FOUND" {
		t.Fatal("delayed result lost correlation")
	}
}
