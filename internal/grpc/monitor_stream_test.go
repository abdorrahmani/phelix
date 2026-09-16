package grpc

import (
	"sync"
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/server"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeMetricsCollector is a minimal, deterministic MetricsCollector used to
// drive the monitor stream in tests without touching the real host/app
// state.
type fakeMetricsCollector struct{}

func (fakeMetricsCollector) CollectAppMetrics() []monitor.AppMetrics {
	return []monitor.AppMetrics{{AppID: "app-1", ServerID: "srv-1", CPUUsage: 1.2, MemoryUsage: 2048}}
}

func (fakeMetricsCollector) CollectAppDetails() []monitor.AppDetails {
	return []monitor.AppDetails{{ID: "app-1", ServerID: "srv-1", Name: "myapp", Status: "running"}}
}

func (fakeMetricsCollector) CollectServerMetrics() (*server.Metrics, error) {
	return &server.Metrics{ServerID: "srv-1", CPUUsagePercent: 5}, nil
}

func (fakeMetricsCollector) CollectAppLogs() ([]logs.LogEntry, error)  { return nil, nil }
func (fakeMetricsCollector) CollectSelfLogs() ([]logs.LogEntry, error) { return nil, nil }

// fakeCommandExecutor records executed commands and lets tests control the
// result.
type fakeCommandExecutor struct {
	mu       sync.Mutex
	executed []monitor.Command
	err      error
}

func (f *fakeCommandExecutor) Execute(cmd monitor.Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executed = append(f.executed, cmd)
	return f.err
}

func (f *fakeCommandExecutor) executedCommands() []monitor.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]monitor.Command, len(f.executed))
	copy(out, f.executed)
	return out
}

// fakeMonitorBackend is a minimal backend implementation of
// PhelixServiceServer that only handles MonitorStream, used to verify the
// CLI <-> backend contract end-to-end over a real (in-memory) gRPC
// connection.
type fakeMonitorBackend struct {
	pb.UnimplementedPhelixServiceServer

	mu       sync.Mutex
	received []*pb.MonitorEvent
	newEvent chan struct{}

	toClient chan *pb.MonitorControl
}

func newFakeMonitorBackend() *fakeMonitorBackend {
	return &fakeMonitorBackend{
		newEvent: make(chan struct{}, 256),
		toClient: make(chan *pb.MonitorControl, 8),
	}
}

func (b *fakeMonitorBackend) MonitorStream(stream pb.PhelixService_MonitorStreamServer) error {
	recvErr := make(chan error, 1)
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			b.mu.Lock()
			b.received = append(b.received, ev)
			b.mu.Unlock()
			select {
			case b.newEvent <- struct{}{}:
			default:
			}
		}
	}()

	for {
		select {
		case err := <-recvErr:
			return err
		case ctrl, ok := <-b.toClient:
			if !ok {
				return nil
			}
			if err := stream.Send(ctrl); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return nil
		}
	}
}

func (b *fakeMonitorBackend) eventsWithPayload(match func(*pb.MonitorEvent) bool, timeout time.Duration) *pb.MonitorEvent {
	deadline := time.After(timeout)
	for {
		b.mu.Lock()
		for _, ev := range b.received {
			if match(ev) {
				b.mu.Unlock()
				return ev
			}
		}
		b.mu.Unlock()

		select {
		case <-b.newEvent:
		case <-deadline:
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestMonitorStream_SendsServerInfoAndMetrics(t *testing.T) {
	setupTestSession(t)

	// Server-level data (ServerInfo, server metrics) flows only while at
	// least one app is watched — the real app.Manager has no watched apps in
	// the test environment, so stub one in.
	watchOneApp(t)

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 20 * time.Millisecond
	defer func() { monitorMetricsInterval = origInterval }()

	origCollector := monitorStream.metricsCollector
	origExecutor := monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = &fakeCommandExecutor{}
	defer func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	}()

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	go func() {
		streamErrCh <- c.runMonitorStream()
	}()

	// Expect a ServerInfo event first.
	infoEv := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
		return ok
	}, 2*time.Second)
	if infoEv == nil {
		t.Fatal("timed out waiting for ServerInfo event")
	}

	// The ServerInfo must carry the seeded server-settings groups end-to-end
	// (connection/alert/security attached by Initialize → converter → wire).
	si := infoEv.GetServerInfo()
	if si.GetConnection() == nil || si.GetAlert() == nil || si.GetSecurity() == nil {
		t.Fatal("expected ServerInfo to carry connection/alert/security settings groups")
	}
	// The connection group is present but always empty: the agent never
	// reports SSH port/user/auth method or key material.
	conn := si.GetConnection()
	if conn.GetSshPort() != 0 || conn.GetSshUser() != "" || conn.GetAuthMethod() != "" ||
		conn.GetSshPassword() != "" || conn.GetPrivateKey() != "" || conn.GetPublicKey() != "" {
		t.Fatalf("connection settings must be empty on the wire, got %+v", conn)
	}
	if si.GetAlert().GetCpuThreshold() != 80 || si.GetSecurity().GetSshRootLogin() != "prohibit-password" {
		t.Fatalf("unexpected seeded alert/security settings: alert=%+v security=%+v", si.GetAlert(), si.GetSecurity())
	}

	// Expect at least one ServerMetrics tick.
	metricsEv := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerMetrics)
		return ok
	}, 2*time.Second)
	if metricsEv == nil {
		t.Fatal("timed out waiting for ServerMetrics event")
	}

	// Expect app metrics and app info too.
	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_AppMetrics)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for AppResourceMetrics event")
	}
	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_AppInfo)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for ApplicationInfo event")
	}

	// Graceful shutdown: closing done should make runMonitorStream return.
	close(c.done)
	select {
	case <-streamErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for monitor stream to shut down")
	}
}

func TestMonitorStream_HandlesBackendCommand(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 200 * time.Millisecond // slow enough to not interfere
	defer func() { monitorMetricsInterval = origInterval }()

	executor := &fakeCommandExecutor{}
	origCollector := monitorStream.metricsCollector
	origExecutor := monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = executor
	defer func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	}()

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	go func() {
		streamErrCh <- c.runMonitorStream()
	}()

	// Wait for the stream to be established (ServerInfo arrives first).
	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for initial ServerInfo event")
	}

	// Push a command from the backend to the CLI.
	backend.toClient <- &pb.MonitorControl{
		Payload: &pb.MonitorControl_Command{
			Command: &pb.MonitorCommandRequest{
				RequestId: "req-1",
				Type:      "restart",
				AppName:   "myapp",
			},
		},
	}

	// The CLI should execute the command and report a result back.
	resultEv := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		res, ok := ev.Payload.(*pb.MonitorEvent_CommandResult)
		return ok && res.CommandResult.GetRequestId() == "req-1"
	}, 3*time.Second)
	if resultEv == nil {
		t.Fatal("timed out waiting for command result event")
	}

	result := resultEv.Payload.(*pb.MonitorEvent_CommandResult).CommandResult
	if result.GetStatus() != "success" {
		t.Fatalf("expected success status, got %q (error=%q)", result.GetStatus(), result.GetError())
	}
	if result.GetCommand() != "restart" || result.GetAppName() != "myapp" {
		t.Fatalf("unexpected result fields: %+v", result)
	}

	executed := executor.executedCommands()
	if len(executed) != 1 || executed[0].Payload.AppName != "myapp" || executed[0].Type != "restart" {
		t.Fatalf("expected executor to receive the command, got %+v", executed)
	}
	// A command without deployment overrides must reach the executor with them
	// unset, so it resolves the strategy from phelix.yaml as before.
	if executed[0].Payload.Strategy != "" || executed[0].Payload.Replicas != 0 {
		t.Fatalf("unexpected deployment overrides on a plain restart: %+v", executed[0].Payload)
	}

	// A rebuild carrying one-off overrides must deliver them to the executor
	// verbatim — this is the whole point of the new wire fields.
	backend.toClient <- &pb.MonitorControl{
		Payload: &pb.MonitorControl_Command{
			Command: &pb.MonitorCommandRequest{
				RequestId: "req-2",
				Type:      "rebuild",
				AppName:   "myapp",
				Strategy:  "rolling",
				Replicas:  3,
			},
		},
	}
	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		res, ok := ev.Payload.(*pb.MonitorEvent_CommandResult)
		return ok && res.CommandResult.GetRequestId() == "req-2"
	}, 5*time.Second); ev == nil {
		t.Fatal("timed out waiting for the override command result event")
	}

	executed = executor.executedCommands()
	if len(executed) != 2 {
		t.Fatalf("expected 2 executed commands, got %+v", executed)
	}
	if got := executed[1].Payload; got.Strategy != "rolling" || got.Replicas != 3 || got.Type != "rebuild" {
		t.Fatalf("overrides did not reach the executor: %+v", got)
	}

	close(c.done)
	select {
	case <-streamErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for monitor stream to shut down")
	}
}

func TestMonitorStream_RespondsToPing(t *testing.T) {
	setupTestSession(t)
	watchOneApp(t)

	origInterval := monitorMetricsInterval
	monitorMetricsInterval = 200 * time.Millisecond
	defer func() { monitorMetricsInterval = origInterval }()

	origCollector := monitorStream.metricsCollector
	origExecutor := monitorStream.commandExecutor
	monitorStream.metricsCollector = fakeMetricsCollector{}
	monitorStream.commandExecutor = &fakeCommandExecutor{}
	defer func() {
		monitorStream.metricsCollector = origCollector
		monitorStream.commandExecutor = origExecutor
	}()

	backend := newFakeMonitorBackend()
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	streamErrCh := make(chan error, 1)
	go func() {
		streamErrCh <- c.runMonitorStream()
	}()

	if ev := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		_, ok := ev.Payload.(*pb.MonitorEvent_ServerInfo)
		return ok
	}, 2*time.Second); ev == nil {
		t.Fatal("timed out waiting for initial ServerInfo event")
	}

	backend.toClient <- &pb.MonitorControl{
		Payload: &pb.MonitorControl_Ping{
			Ping: &pb.Ping{Id: "ping-1", Timestamp: time.Now().UnixMilli()},
		},
	}

	pongEv := backend.eventsWithPayload(func(ev *pb.MonitorEvent) bool {
		pong, ok := ev.Payload.(*pb.MonitorEvent_Pong)
		return ok && pong.Pong.GetId() == "ping-1"
	}, 2*time.Second)
	if pongEv == nil {
		t.Fatal("timed out waiting for pong response")
	}

	close(c.done)
	select {
	case <-streamErrCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for monitor stream to shut down")
	}
}

func TestMonitorStreamManager_PauseResume(t *testing.T) {
	s := &monitorStreamManager{}
	if s.isPaused() {
		t.Fatal("expected not paused initially")
	}
	s.pause()
	if !s.isPaused() {
		t.Fatal("expected paused after pause()")
	}
	s.resume()
	if s.isPaused() {
		t.Fatal("expected not paused after resume()")
	}
}

func TestMonitorStreamManager_SendWithoutStream(t *testing.T) {
	s := &monitorStreamManager{}
	err := s.send(&pb.MonitorEvent{})
	if err == nil {
		t.Fatal("expected error when sending without an active stream")
	}
}
