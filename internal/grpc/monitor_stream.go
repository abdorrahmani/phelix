package grpc

import (
	"context"
	"io"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/protobuf/proto"
)

// monitorMetricsInterval controls how often the monitor daemon sends a
// snapshot of server/app metrics through the stream. This preserves the
// ~2s cadence of the legacy WebSocket monitor. It is a var (not a const) so
// tests can shorten it instead of waiting on the real interval.
var monitorMetricsInterval = 2 * time.Second

const monitorCommandSettleDelay = 2 * time.Second

// monitorStreamRetryDelay is how long to wait before re-opening the
// monitor stream after it ends (the underlying gRPC connection's own
// reconnect/backoff logic — see reconnect.go — governs connection-level
// retries; this is just the inter-stream-attempt pause on an otherwise
// healthy connection).
const monitorStreamRetryDelay = 2 * time.Second

const maxRemoteVerifyDurationMS = int64((30 * time.Minute) / time.Millisecond)

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

// sendCommandResult wraps a MonitorCommandResult in a MonitorEvent. Delivery
// errors are returned so durable rollback results remain pending for replay on
// the next MonitorStream connection.
func (s *monitorStreamManager) sendCommandResult(result *pb.MonitorCommandResult) error {
	event := &pb.MonitorEvent{
		ServerId:  server.GetServerID(),
		Timestamp: time.Now().UnixMilli(),
		Payload: &pb.MonitorEvent_CommandResult{
			CommandResult: result,
		},
	}
	return s.send(event)
}

// cloneMonitorCommandResult deep-copies a cached result so a replay never
// aliases (and never reuses) the stored message. proto.Clone is required:
// plain struct copy would duplicate the embedded protoimpl.MessageState
// (which holds a noCopy mutex) and alias the map/slice fields.
func cloneMonitorCommandResult(r *pb.MonitorCommandResult) *pb.MonitorCommandResult {
	return proto.Clone(r).(*pb.MonitorCommandResult)
}

func (s *monitorStreamManager) send(event *pb.MonitorEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream == nil {
		return phelixerr.New(phelixerr.CodeConnection, "monitor stream is not connected")
	}
	if err := s.stream.Send(event); err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "failed to send monitor event", err)
	}
	return nil
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
	logs.InfoFile("grpc", "[gRPC Monitor] monitor stream loop started")
	for {
		select {
		case <-c.done:
			logs.InfoFile("grpc", "[gRPC Monitor] monitor stream loop stopped")
			return
		default:
		}

		if !c.IsConnected() {
			time.Sleep(1 * time.Second)
			continue
		}

		if err := c.runMonitorStream(); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] stream disconnected: %v", err)
			c.reconnectIfNeeded()
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
		return phelixerr.Wrap(phelixerr.CodeConnection, "attach auth metadata for monitor stream", err)
	}

	stream, err := c.serviceClient.MonitorStream(authCtx)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "open monitor stream", err)
	}

	monitorStream.mu.Lock()
	monitorStream.stream = stream
	monitorStream.cancel = cancel
	monitorStream.mu.Unlock()

	defer func() {
		monitorStream.mu.Lock()
		if monitorStream.stream == stream {
			monitorStream.stream = nil
			monitorStream.cancel = nil
		}
		monitorStream.mu.Unlock()
	}()

	logs.InfoFile("grpc", "[gRPC Monitor] monitor stream connected (agent_id=%s)", server.GetAgentID())

	ledger := rollbackResults
	matrixLedger := matrixResults
	executor := monitorStream.commandExecutor
	if err := ledger.initialize(); err != nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] remote rollback ledger unavailable; rollback commands will fail closed: %v", err)
	} else {
		c.replayPendingRollbackResults(ledger)
	}
	if err := matrixLedger.initialize(); err != nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] remote matrix ledger unavailable; matrix commands will fail closed: %v", err)
	} else {
		c.replayPendingMatrixResults(matrixLedger)
	}

	// Send server identity once per (re)connection, mirroring the legacy
	// WebSocket "servers" message sent on connect/reconnect.
	if err := c.sendMonitorServerInfo(); err != nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] failed to send server info: %v", err)
	}

	// Deployment state resync: pushed on every (re)connection so a backend that
	// missed a deployment's events — it restarted, or the connection was down
	// while the deploy ran — converges on the real topology instead of waiting
	// for the next deploy.
	c.sendDeploymentSnapshots()

	// Health resync, same reason and same shape: a full snapshot per app on
	// every (re)connection is what makes health configuration changes and
	// deletions durable across a disconnect. Nothing else carries them, so this
	// must run on every stream open, not just the first.
	c.sendHealthSnapshots()

	// Matrix resync: the full state of every ACTIVE run (execution lock held),
	// so a backend that missed a run's lifecycle events converges on the real
	// state. Runs that are not executing are not resynced — they are durable
	// history, queryable with a matrix_status command.
	c.sendMatrixSnapshots()

	recvErrCh := make(chan error, 1)
	go func() {
		recvErrCh <- c.monitorRecvLoop(stream, executor, ledger, matrixLedger)
	}()

	ticker := time.NewTicker(monitorMetricsInterval)
	defer ticker.Stop()

	// Deployment topology changes only during a deploy, which reports its own
	// events; the periodic snapshot is a slow safety net against divergence,
	// not a metrics feed. Active matrix runs ride the same cadence: their
	// per-combination state changes through events, and this snapshot is the
	// reconnect-independent safety net.
	deployTicker := time.NewTicker(deploymentResyncInterval)
	defer deployTicker.Stop()

	// Health, by contrast, changes on its own as endpoints are probed, so its
	// snapshot IS the update channel — at the endpoint check cadence, not the
	// metrics cadence.
	healthTicker := time.NewTicker(healthSnapshotInterval)
	defer healthTicker.Stop()

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
		case <-deployTicker.C:
			if monitorStream.isPaused() {
				continue
			}
			c.sendDeploymentSnapshots()
			c.sendMatrixSnapshots()
		case <-healthTicker.C:
			if monitorStream.isPaused() {
				continue
			}
			c.sendHealthSnapshots()
		}
	}
}

// monitorRecvLoop reads MonitorControl messages pushed by the backend
// (remote commands, keepalive pings) until the stream ends.
func (c *Client) monitorRecvLoop(stream pb.PhelixService_MonitorStreamClient, executor monitor.CommandExecutor, ledger *rollbackLedger, matrixLedger *rollbackLedger) error {
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			logs.InfoFile("grpc", "[gRPC Monitor] stream closed by backend")
			return nil
		}
		if err != nil {
			return err
		}

		switch p := msg.Payload.(type) {
		case *pb.MonitorControl_Command:
			// Matrix commands have their own dispatch: async execution, a
			// separate ledger, and structured results. They never reach the
			// lifecycle executor.
			if isMatrixCommand(p.Command.GetType()) {
				c.handleMatrixCommand(p.Command, matrixLedger)
				continue
			}
			c.handleMonitorCommand(p.Command, executor, ledger)
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
		logs.ErrorFile("grpc", "[gRPC Monitor] failed to send pong: %v", err)
	}
}

func (c *Client) replayPendingRollbackResults(ledger *rollbackLedger) {
	results, err := ledger.pendingResults()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] cannot load pending rollback results: %v", err)
		return
	}
	for _, result := range results {
		if err := monitorStream.sendCommandResult(result); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to replay rollback result request_id=%s: %v", result.GetRequestId(), err)
			return
		}
		if err := ledger.markDelivered(result.GetRequestId()); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to mark rollback result delivered request_id=%s: %v", result.GetRequestId(), err)
			return
		}
	}
}

func commandErrorResult(req *pb.MonitorCommandRequest, err error) *pb.MonitorCommandResult {
	result := &pb.MonitorCommandResult{
		RequestId: req.GetRequestId(),
		Command:   req.GetType(),
		AppName:   req.GetAppName(),
		Status:    "error",
		Error:     err.Error(),
		Timestamp: time.Now().UnixMilli(),
	}
	if structured := phelixerr.AsError(err); structured != nil {
		result.ErrorCode = string(structured.Code)
	} else {
		result.ErrorCode = string(phelixerr.CodeUnknown)
	}
	return result
}

func sendCommandResultLogged(result *pb.MonitorCommandResult) error {
	if err := monitorStream.sendCommandResult(result); err != nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] failed to send command result request_id=%s: %v", result.GetRequestId(), err)
		return err
	}
	return nil
}

// handleMonitorCommand executes a backend-issued remote command against a
// managed application, exactly like the legacy WebSocket "command" message
// handling: metrics are paused during execution, a result is sent back, then
// metrics resume and an immediate refreshed snapshot is pushed.
func (c *Client) handleMonitorCommand(req *pb.MonitorCommandRequest, executor monitor.CommandExecutor, ledger *rollbackLedger) {
	if req == nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] received nil command")
		return
	}
	logs.InfoFile("grpc", "[gRPC Monitor] received command: type=%s app=%s", req.GetType(), req.GetAppName())

	isRollback := req.GetType() == monitor.CommandRollback
	if isRollback {
		if req.GetRequestId() == "" {
			_ = sendCommandResultLogged(commandErrorResult(req,
				phelixerr.New(phelixerr.CodeInvalidArgument, "rollback command requires a request_id")))
			return
		}
		rawMS := req.GetVerifyDurationMs()
		if rawMS < 0 || rawMS > maxRemoteVerifyDurationMS {
			_ = sendCommandResultLogged(commandErrorResult(req, phelixerr.Newf(
				phelixerr.CodeInvalidArgument,
				"invalid verify_duration_ms %d: must be between 0 and %d",
				rawMS, maxRemoteVerifyDurationMS)))
			return
		}
		outcome, replay, err := ledger.begin(req)
		if err != nil {
			_ = sendCommandResultLogged(commandErrorResult(req, err))
			return
		}
		if outcome == rollbackBeginReplay {
			if sendCommandResultLogged(replay) == nil {
				if err := ledger.markDelivered(req.GetRequestId()); err != nil {
					logs.ErrorFile("grpc", "[gRPC Monitor] failed to mark replay delivered: %v", err)
				}
			}
			return
		}
	}

	if req.GetStrategy() != "" || req.GetReplicas() != 0 {
		logs.InfoFile("grpc", "[gRPC Monitor] one-off deployment override: strategy=%s replicas=%d",
			req.GetStrategy(), req.GetReplicas())
	}

	monitorStream.pause()
	defer monitorStream.resume()

	cmd := monitor.Command{
		Type: req.GetType(),
		Payload: monitor.CommandPayload{
			Type:           req.GetType(),
			AppName:        req.GetAppName(),
			RequestID:      req.GetRequestId(),
			Strategy:       req.GetStrategy(),
			Replicas:       int(req.GetReplicas()),
			Target:         req.GetTarget(),
			Reason:         req.GetReason(),
			VerifyDuration: time.Duration(req.GetVerifyDurationMs()) * time.Millisecond,
			DryRun:         req.GetDryRun(),
		},
	}

	result := &pb.MonitorCommandResult{
		RequestId: req.GetRequestId(),
		Command:   req.GetType(),
		AppName:   req.GetAppName(),
		Status:    "success",
		Timestamp: time.Now().UnixMilli(),
	}
	if err := executor.Execute(cmd); err != nil {
		result = commandErrorResult(req, err)
		logs.ErrorFile("grpc", "[gRPC Monitor] command execution failed: %v", err)
	}

	if isRollback {
		if err := ledger.complete(req.GetRequestId(), result); err != nil {
			result = commandErrorResult(req, phelixerr.Wrap(phelixerr.CodeUnavailable,
				"rollback completed but its durable result could not be recorded", err))
			logs.ErrorFile("grpc", "[gRPC Monitor] rollback result persistence failed: %v", err)
		} else if sendCommandResultLogged(result) == nil {
			if err := ledger.markDelivered(req.GetRequestId()); err != nil {
				logs.ErrorFile("grpc", "[gRPC Monitor] failed to mark rollback result delivered: %v", err)
			}
		}
	} else {
		_ = sendCommandResultLogged(result)
	}

	// Give the command a moment to fully settle before resuming metrics,
	// mirroring the legacy WebSocket behavior. Stream shutdown cancels the wait
	// so an old receive loop cannot retain mutable command dependencies.
	timer := time.NewTimer(monitorCommandSettleDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-c.done:
		return
	}
	logs.InfoFile("grpc", "[gRPC Monitor] metrics resumed after command execution")
	c.sendMonitorTick()
}

// sendMonitorServerInfo sends the ServerInfo snapshot once per stream
// (re)connection, mirroring the legacy "servers" WebSocket message.
func (c *Client) sendMonitorServerInfo() error {
	info := server.GetServerInfo()
	if info == nil {
		return phelixerr.New(phelixerr.CodeServer, "server info not initialized")
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
		logs.ErrorFile("grpc", "[gRPC Monitor] failed to collect server metrics: %v", err)
	} else {
		event := &pb.MonitorEvent{
			ServerId:  serverID,
			Timestamp: time.Now().UnixMilli(),
			Payload: &pb.MonitorEvent_ServerMetrics{
				ServerMetrics: toProtoServerMetrics(metrics),
			},
		}
		if err := monitorStream.send(event); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to send server metrics: %v", err)
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
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to send app metrics: %v", err)
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
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to send app info: %v", err)
			return
		}
	}

	if entries, err := monitorStream.metricsCollector.CollectAppLogs(); err != nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] failed to collect app logs: %v", err)
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
				logs.ErrorFile("grpc", "[gRPC Monitor] failed to send app log: %v", err)
				return
			}
		}
	}

	if entries, err := monitorStream.metricsCollector.CollectSelfLogs(); err != nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] failed to collect self logs: %v", err)
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
				logs.ErrorFile("grpc", "[gRPC Monitor] failed to send self log: %v", err)
				return
			}
		}
	}
}

// sendMatrixSnapshots pushes the full state of every ACTIVE matrix run (a
// run whose execution lock is held by a live process) as MonitorEvent
// matrix_run payloads. Pushed on every (re)connection and on the deployment
// resync cadence, this is the convergence surface for a backend that missed
// a run's lifecycle events. Runs that are not executing — finished history,
// or "running" records orphaned by a crash — are NOT resynced; the backend
// learns about them from command results and matrix_status queries.
func (c *Client) sendMatrixSnapshots() {
	runs, err := matrix.ActiveRuns()
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC Monitor] failed to collect active matrix runs: %v", err)
		return
	}
	for _, run := range runs {
		event := &pb.MonitorEvent{
			ServerId:  server.GetServerID(),
			Timestamp: time.Now().UnixMilli(),
			Payload: &pb.MonitorEvent_MatrixRun{
				MatrixRun: ToProtoMatrixRunState(run),
			},
		}
		if err := monitorStream.send(event); err != nil {
			logs.ErrorFile("grpc", "[gRPC Monitor] failed to send matrix run snapshot %s: %v", run.ID, err)
			return
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
