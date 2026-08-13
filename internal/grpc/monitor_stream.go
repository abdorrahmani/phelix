package grpc

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/server"
)

// monitorMetricsInterval controls how often the monitor daemon sends a
// snapshot of server/app metrics through the stream. This preserves the
// ~2s cadence of the legacy WebSocket monitor. It is a var (not a const) so
// tests can shorten it instead of waiting on the real interval.
var monitorMetricsInterval = 2 * time.Second

// monitorStreamRetryDelay is how long to wait before re-opening the
// monitor stream after it ends (the underlying gRPC connection's own
// reconnect/backoff logic — see reconnect.go — governs connection-level
// retries; this is just the inter-stream-attempt pause on an otherwise
// healthy connection).
const monitorStreamRetryDelay = 2 * time.Second

// monitorStreamManager owns the single, long-lived MonitorStream and
// serializes writes to it (gRPC streams do not support concurrent Send
// calls from multiple goroutines).
type monitorStreamManager struct {
	mu     sync.Mutex
	stream pb.PhelixService_MonitorStreamClient
	cancel context.CancelFunc

	metricsCollector monitor.MetricsCollector
	commandExecutor  monitor.CommandExecutor

	pausedMu sync.Mutex
	paused   bool
}

var monitorStream = &monitorStreamManager{
	metricsCollector: monitor.NewMetricsCollector(),
	commandExecutor:  monitor.NewCommandExecutor(),
}

func (s *monitorStreamManager) send(event *pb.MonitorEvent) error {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()

	if stream == nil {
		return fmt.Errorf("monitor stream is not connected")
	}
	return stream.Send(event)
}

func (s *monitorStreamManager) pause() {
	s.pausedMu.Lock()
	s.paused = true
	s.pausedMu.Unlock()
}

func (s *monitorStreamManager) resume() {
	s.pausedMu.Lock()
	s.paused = false
	s.pausedMu.Unlock()
}

func (s *monitorStreamManager) isPaused() bool {
	s.pausedMu.Lock()
	defer s.pausedMu.Unlock()
	return s.paused
}

// StartMonitorStream launches the persistent monitoring stream loop in the
// background. It never blocks the caller and never crashes the process: any
// connection failure is logged and retried, exactly like the legacy
// WebSocket monitor's reconnect behavior.
func (c *Client) StartMonitorStream() {
	go c.monitorStreamLoop()
}

// monitorStreamLoop keeps a monitor stream open for as long as the client is
// alive, re-opening it whenever it ends (backend restart, network blip,
// etc.). It relies on the Client's own connection-level reconnect/backoff
// (see reconnect.go) to recover the underlying gRPC connection; here we only
// need to re-open the logical stream once the connection is healthy again.
func (c *Client) monitorStreamLoop() {
	grpcLog("[gRPC Monitor] monitor stream loop started")
	for {
		select {
		case <-c.done:
			grpcLog("[gRPC Monitor] monitor stream loop stopped")
			return
		default:
		}

		if !c.IsConnected() {
			time.Sleep(1 * time.Second)
			continue
		}

		if err := c.runMonitorStream(); err != nil {
			grpcLog("[gRPC Monitor] stream disconnected: %v", err)
		}

		select {
		case <-c.done:
			return
		case <-time.After(monitorStreamRetryDelay):
		}
	}
}

// runMonitorStream opens a single MonitorStream session, sends the initial
// ServerInfo snapshot, then drives both the periodic metrics sender and the
// backend command/ping receiver until the stream ends or the client is
// closed.
func (c *Client) runMonitorStream() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		return fmt.Errorf("auth metadata: %w", err)
	}

	stream, err := c.serviceClient.MonitorStream(authCtx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	monitorStream.mu.Lock()
	monitorStream.stream = stream
	monitorStream.cancel = cancel
	monitorStream.mu.Unlock()

	defer func() {
		monitorStream.mu.Lock()
		monitorStream.stream = nil
		monitorStream.cancel = nil
		monitorStream.mu.Unlock()
	}()

	grpcLog("[gRPC Monitor] monitor stream connected")

	// Send server identity once per (re)connection, mirroring the legacy
	// WebSocket "servers" message sent on connect/reconnect.
	if err := c.sendMonitorServerInfo(); err != nil {
		grpcLog("[gRPC Monitor] failed to send server info: %v", err)
	}

	recvErrCh := make(chan error, 1)
	go func() {
		recvErrCh <- c.monitorRecvLoop(stream)
	}()

	ticker := time.NewTicker(monitorMetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return nil
		case err := <-recvErrCh:
			return err
		case <-ticker.C:
			if monitorStream.isPaused() {
				continue
			}
			c.sendMonitorTick()
		}
	}
}

// monitorRevLoop reads MonitorControl messages pushed by the backend
// (remote commands, keepalive pings) until the stream ends.
func (c *Client) monitorRecvLoop(stream pb.PhelixService_MonitorStreamClient) error {
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			grpcLog("[gRPC Monitor] stream closed by backend")
			return nil
		}
		if err != nil {
			return err
		}

		switch p := msg.Payload.(type) {
		case *pb.MonitorControl_Command:
			c.handleMonitorCommand(p.Command)
		case *pb.MonitorControl_Ping:
			c.handleMonitorPing(p.Ping)
		}
	}
}

// handleMonitorPing responds to a backend keepalive ping at the application
// level. Transport-level liveness is additionally covered by gRPC keepalive
// (see client.go), but the response is preserved for backend compatibility
// with the legacy WebSocket ping/pong exchange.
func (c *Client) handleMonitorPing(ping *pb.Ping) {
	event := &pb.MonitorEvent{
		ServerId:  server.GetServerID(),
		Timestamp: time.Now().UnixMilli(),
		Payload: &pb.MonitorEvent_Pong{
			Pong: &pb.Pong{
				Id:        ping.GetId(),
				Timestamp: time.Now().UnixMilli(),
			},
		},
	}
	if err := monitorStream.send(event); err != nil {
		grpcLog("[gRPC Monitor] failed to send pong: %v", err)
	}
}

// handleMonitorCommand executes a backend-issued remote command against a
// managed application, exactly like the legacy WebSocket "command" message
// handling: metrics are paused during execution, a result is sent back, then
// metrics resume and an immediate refreshed snapshot is pushed.
func (c *Client) handleMonitorCommand(req *pb.MonitorCommandRequest) {
	grpcLog("[gRPC Monitor] received command: type=%s app=%s", req.GetType(), req.GetAppName())

	monitorStream.pause()

	cmd := monitor.Command{
		Type: req.GetType(),
		Payload: monitor.CommandPayload{
			Type:    req.GetType(),
			AppName: req.GetAppName(),
		},
	}

	result := &pb.MonitorCommandResult{
		RequestId: req.GetRequestId(),
		Command:   req.GetType(),
		AppName:   req.GetAppName(),
		Status:    "success",
		Timestamp: time.Now().UnixMilli(),
	}

	if err := monitorStream.commandExecutor.Execute(cmd); err != nil {
		result.Status = "error"
		result.Error = err.Error()
		grpcLog("[gRPC Monitor] command execution failed: %v", err)
	}

	event := &pb.MonitorEvent{
		ServerId:  server.GetServerID(),
		Timestamp: time.Now().UnixMilli(),
		Payload: &pb.MonitorEvent_CommandResult{
			CommandResult: result,
		},
	}
	if err := monitorStream.send(event); err != nil {
		grpcLog("[gRPC Monitor] failed to send command result: %v", err)
	}

	// Give the command a moment to fully settle before resuming metrics,
	// mirroring the legacy WebSocket behavior.
	time.Sleep(2 * time.Second)
	monitorStream.resume()
	grpcLog("[gRPC Monitor] metrics resumed after command execution")

	// Force an immediate refreshed snapshot after the command completes.
	go c.sendMonitorTick()
}

// sendMonitorServerInfo sends the ServerInfo snapshot once per stream
// (re)connection, mirroring the legacy "servers" WebSocket message.
func (c *Client) sendMonitorServerInfo() error {
	info := server.GetServerInfo()
	if info == nil {
		return fmt.Errorf("server info not initialized")
	}
	event := &pb.MonitorEvent{
		ServerId:  server.GetServerID(),
		Timestamp: time.Now().UnixMilli(),
		Payload: &pb.MonitorEvent_ServerInfo{
			ServerInfo: toProtoServerInfo(info),
		},
	}
	return monitorStream.send(event)
}

// sendMonitorTick sends one full snapshot of server metrics, per-app
// resource usage, per-app details, and logs — mirroring the legacy
// WebSocket sendMetrics() tick (server_metrics + metrics + apps + app_logs +
// self_logs), sent every monitorMetricsInterval.
func (c *Client) sendMonitorTick() {
	serverID := server.GetServerID()

	if metrics, err := monitorStream.metricsCollector.CollectServerMetrics(); err != nil {
		grpcLog("[gRPC Monitor] failed to collect server metrics: %v", err)
	} else {
		event := &pb.MonitorEvent{
			ServerId:  serverID,
			Timestamp: time.Now().UnixMilli(),
			Payload: &pb.MonitorEvent_ServerMetrics{
				ServerMetrics: toProtoServerMetrics(metrics),
			},
		}
		if err := monitorStream.send(event); err != nil {
			grpcLog("[gRPC Monitor] failed to send server metrics: %v", err)
			return
		}
	}

	for _, m := range monitorStream.metricsCollector.CollectAppMetrics() {
		event := &pb.MonitorEvent{
			ServerId:  serverID,
			Timestamp: time.Now().UnixMilli(),
			Payload: &pb.MonitorEvent_AppMetrics{
				AppMetrics: toProtoAppMetrics(m),
			},
		}
		if err := monitorStream.send(event); err != nil {
			grpcLog("[gRPC Monitor] failed to send app metrics: %v", err)
			return
		}
	}

	for _, a := range monitorStream.metricsCollector.CollectAppDetails() {
		event := &pb.MonitorEvent{
			ServerId:  serverID,
			Timestamp: time.Now().UnixMilli(),
			Payload: &pb.MonitorEvent_AppInfo{
				AppInfo: toProtoAppInfo(a),
			},
		}
		if err := monitorStream.send(event); err != nil {
			grpcLog("[gRPC Monitor] failed to send app info: %v", err)
			return
		}
	}

	if entries, err := monitorStream.metricsCollector.CollectAppLogs(); err != nil {
		grpcLog("[gRPC Monitor] failed to collect app logs: %v", err)
	} else {
		for _, e := range entries {
			event := &pb.MonitorEvent{
				ServerId:  serverID,
				Timestamp: time.Now().UnixMilli(),
				Payload: &pb.MonitorEvent_LogEntry{
					LogEntry: toProtoLogEntry(e, pb.LogSource_LOG_SOURCE_APPLICATION),
				},
			}
			if err := monitorStream.send(event); err != nil {
				grpcLog("[gRPC Monitor] failed to send app log: %v", err)
				return
			}
		}
	}

	if entries, err := monitorStream.metricsCollector.CollectSelfLogs(); err != nil {
		grpcLog("[gRPC Monitor] failed to collect self logs: %v", err)
	} else {
		for _, e := range entries {
			event := &pb.MonitorEvent{
				ServerId:  serverID,
				Timestamp: time.Now().UnixMilli(),
				Payload: &pb.MonitorEvent_LogEntry{
					LogEntry: toProtoLogEntry(e, pb.LogSource_LOG_SOURCE_SELF),
				},
			}
			if err := monitorStream.send(event); err != nil {
				grpcLog("[gRPC Monitor] failed to send self log: %v", err)
				return
			}
		}
	}
}

// stopMonitorStream cancels the active monitor stream context, if any. Used
// during graceful shutdown.
func stopMonitorStream() {
	monitorStream.mu.Lock()
	defer monitorStream.mu.Unlock()
	if monitorStream.cancel != nil {
		monitorStream.cancel()
	}
	monitorStream.stream = nil
}
