package grpc

import (
	"context"
	"io"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
	"google.golang.org/grpc/status"
)

// agentStreamManager manages the bidirectional AgentStream for backend-to-CLI commands.
type agentStreamManager struct {
	mu     sync.RWMutex
	stream pb.PhelixService_AgentStreamClient
	cancel context.CancelFunc
	// activeWatches maps a watch's request_id to its cancel + generation. The
	// generation lets a watch goroutine remove ONLY its own entry on exit, so a
	// duplicate request_id that replaced it is not clobbered.
	activeWatches map[string]activeWatch
	nextWatchGen  uint64
	// sendMu serializes Send on the gRPC stream. Sends now come from both the
	// recv loop (command results, pongs) and watch goroutines; a gRPC stream does
	// not allow concurrent Send.
	sendMu sync.Mutex
}

// activeWatch is one live watch's cancellation handle plus a generation tag.
type activeWatch struct {
	cancel context.CancelFunc
	gen    uint64
}

var agentStream = &agentStreamManager{
	activeWatches: make(map[string]activeWatch),
}

// sendMsg serializes writes to the agent stream (one Send at a time).
func (m *agentStreamManager) sendMsg(stream pb.PhelixService_AgentStreamClient, msg *pb.ClientToServer) error {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	return stream.Send(msg)
}

// registerWatch stores a watch's cancel keyed by request_id, cancelling and
// replacing any prior watch under the same id, and returns the generation the
// goroutine passes back to removeWatch on exit.
func (m *agentStreamManager) registerWatch(requestID string, cancel context.CancelFunc) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.activeWatches[requestID]; ok {
		prev.cancel()
	}
	m.nextWatchGen++
	gen := m.nextWatchGen
	m.activeWatches[requestID] = activeWatch{cancel: cancel, gen: gen}
	return gen
}

// removeWatch drops a watch's entry only if it is still the generation that
// registered it (a newer duplicate may have replaced it).
func (m *agentStreamManager) removeWatch(requestID string, gen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.activeWatches[requestID]; ok && cur.gen == gen {
		delete(m.activeWatches, requestID)
	}
}

// startAgentStream opens the bidirectional stream and processes incoming commands.
//
// Like the monitor stream loop, it stops entirely when the backend actively
// rejects the credentials (gRPC Unauthenticated) — retrying a revoked token
// every 2s only hammers the backend — and parks while no valid local session
// exists instead of spinning on an attach that can never succeed.
func (c *Client) startAgentStream() {
	// Consecutive stream-rate budget terminations (E1), escalating the pause
	// like the monitor stream loop does.
	rateStrikes := 0

	for {
		select {
		case <-c.done:
			return
		default:
		}

		if !c.IsConnected() {
			time.Sleep(1 * time.Second)
			continue
		}

		if !sessionAvailable() {
			select {
			case <-c.done:
				return
			case <-time.After(monitorStreamNoSessionDelay):
			}
			continue
		}

		if err := c.openAgentStream(); err != nil {
			if isAuthRejection(err) {
				logs.ErrorFile("grpc", "[gRPC Agent] backend rejected credentials on agent stream: %v", err)
				c.handleAuthRejected()
				return
			}

			// Backend rate-limit budgets (E1–E3): back off for the budget's
			// window instead of the normal 2s cadence — a fast reopen is
			// exactly the pattern the budgets guard against.
			policy := classifyStreamError(err)
			if policy.reason != "" {
				delay := policy.delay
				if resourceExhaustedKind(err) == "stream_rate" {
					rateStrikes++
					delay = streamRateEscalation(rateStrikes)
				} else {
					rateStrikes = 0
				}
				logs.ErrorFile("grpc", "[gRPC Agent] %s: %v — next attempt in %s",
					policy.reason, err, delay)
				select {
				case <-c.done:
					return
				case <-time.After(delay):
				}
				continue
			}

			rateStrikes = 0
			logs.ErrorFile("grpc", "[gRPC Agent] Stream error: %v, reconnecting...", err)
			c.reconnectIfNeeded()
			time.Sleep(2 * time.Second)
			continue
		}

		rateStrikes = 0
	}
}

// openAgentStream opens a single AgentStream session and processes commands until it ends.
func (c *Client) openAgentStream() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "attach auth metadata for agent stream", err)
	}

	stream, err := c.serviceClient.AgentStream(authCtx)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConnection, "open agent stream", err)
	}

	agentStream.mu.Lock()
	agentStream.stream = stream
	agentStream.cancel = cancel
	agentStream.mu.Unlock()

	logs.InfoFile("grpc", "[gRPC Agent] AgentStream connected (agent_id=%s)", server.GetAgentID())

	// Send initial pong to announce presence
	if err := agentStream.sendMsg(stream, &pb.ClientToServer{
		Payload: &pb.ClientToServer_Pong{
			Pong: &pb.Pong{
				Id:        serverID,
				Timestamp: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		// A failed Send on a server-rejected stream carries no gRPC status
		// (typically io.EOF); the actual cause — e.g. Unauthenticated for a
		// revoked session — only surfaces on Recv. Harvest it so the loop can
		// distinguish auth rejections from transport failures.
		if _, recvErr := stream.Recv(); recvErr != nil {
			if _, ok := status.FromError(recvErr); ok {
				return phelixerr.Wrap(phelixerr.CodeConnection, "announce presence on agent stream", recvErr)
			}
		}
		return phelixerr.Wrap(phelixerr.CodeConnection, "announce presence on agent stream", err)
	}

	// Process incoming commands
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			logs.InfoFile("grpc", "[gRPC Agent] Stream closed by backend")
			return nil
		}
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeConnection, "receive from agent stream", err)
		}

		c.handleServerMessage(stream, msg)
	}
}

// handleServerMessage processes a ServerToClient message from the backend.
func (c *Client) handleServerMessage(stream pb.PhelixService_AgentStreamClient, msg *pb.ServerToClient) {
	switch p := msg.Payload.(type) {
	case *pb.ServerToClient_HealthCommand:
		c.handleHealthCommand(stream, p.HealthCommand)
	case *pb.ServerToClient_Cancel:
		c.handleCancelRequest(p.Cancel)
	case *pb.ServerToClient_Ping:
		c.handlePing(stream, p.Ping)
	}
}

// handleHealthCommand processes a health command and sends the result back.
func (c *Client) handleHealthCommand(stream pb.PhelixService_AgentStreamClient, cmd *pb.HealthCommand) {
	logs.InfoFile("grpc", "[gRPC Agent] Health command: request_id=%s", cmd.GetRequestId())

	// Watch streams asynchronously so the recv loop stays responsive (a
	// CancelRequest or another command can arrive while a watch runs).
	if watchCmd := cmd.GetWatch(); watchCmd != nil {
		c.handleHealthWatch(stream, cmd, watchCmd)
		return
	}

	result := ProcessHealthCommand(cmd)
	if err := agentStream.sendMsg(stream, &pb.ClientToServer{
		Payload: &pb.ClientToServer_HealthResult{HealthResult: result},
	}); err != nil {
		logs.ErrorFile("grpc", "[gRPC Agent] Failed to send health result: %v", err)
	}
}

// handleHealthWatch starts a health watch and streams updates on a background
// goroutine. It registers the watch for cancellation, sends the initial status,
// then relays updates until the context is cancelled (stream death or an in-band
// CancelRequest) or the app's updates stop. The listener is always removed when
// the goroutine exits (via ProcessHealthWatch's cleanup), so a watch never leaks.
func (c *Client) handleHealthWatch(stream pb.PhelixService_AgentStreamClient, cmd *pb.HealthCommand, watchCmd *pb.HealthWatchCommand) {
	updateChan, cleanup, err := ProcessHealthWatch(watchCmd.GetAppId(), watchCmd.GetAppName())
	if err != nil {
		result := &pb.HealthResult{
			RequestId: cmd.GetRequestId(),
			ServerId:  cmd.GetServerId(),
			Command:   "health_watch",
			Success:   false,
			Timestamp: time.Now().UnixMilli(),
			Error:     phelixerr.Redact(err.Error()),
			ErrorCode: string(phelixerr.CodeOf(err)),
		}
		_ = agentStream.sendMsg(stream, &pb.ClientToServer{
			Payload: &pb.ClientToServer_HealthResult{HealthResult: result},
		})
		return
	}

	ctx, cancel := context.WithCancel(stream.Context())
	gen := agentStream.registerWatch(cmd.GetRequestId(), cancel)

	go func() {
		defer cleanup()
		defer agentStream.removeWatch(cmd.GetRequestId(), gen)
		defer cancel()

		// Initial status frame, then live updates.
		initial := ProcessHealthCommand(cmd)
		if err := agentStream.sendMsg(stream, &pb.ClientToServer{
			Payload: &pb.ClientToServer_HealthResult{HealthResult: initial},
		}); err != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-updateChan:
				if !ok {
					return
				}
				update.RequestId = cmd.GetRequestId()
				update.ServerId = cmd.GetServerId()
				if err := agentStream.sendMsg(stream, &pb.ClientToServer{
					Payload: &pb.ClientToServer_HealthUpdate{HealthUpdate: update},
				}); err != nil {
					logs.ErrorFile("grpc", "[gRPC Agent] Failed to send health update: %v", err)
					return
				}
			}
		}
	}()
}

// handleCancelRequest cancels an ongoing operation.
func (c *Client) handleCancelRequest(cancel *pb.CancelRequest) {
	agentStream.mu.Lock()
	defer agentStream.mu.Unlock()

	if w, ok := agentStream.activeWatches[cancel.GetRequestId()]; ok {
		logs.InfoFile("grpc", "[gRPC Agent] Cancelling request: %s", cancel.GetRequestId())
		w.cancel()
		delete(agentStream.activeWatches, cancel.GetRequestId())
	}
}

// handlePing responds to a keepalive ping.
func (c *Client) handlePing(stream pb.PhelixService_AgentStreamClient, ping *pb.Ping) {
	if err := agentStream.sendMsg(stream, &pb.ClientToServer{
		Payload: &pb.ClientToServer_Pong{
			Pong: &pb.Pong{
				Id:        ping.GetId(),
				Timestamp: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		logs.ErrorFile("grpc", "[gRPC Agent] Failed to send pong: %v", err)
	}
}

// stopAgentStream closes the agent stream and cancels all active watches.
func (c *Client) stopAgentStream() {
	agentStream.mu.Lock()
	defer agentStream.mu.Unlock()

	// Cancel all active watches
	for id, w := range agentStream.activeWatches {
		w.cancel()
		delete(agentStream.activeWatches, id)
	}

	// Cancel the stream context
	if agentStream.cancel != nil {
		agentStream.cancel()
	}

	agentStream.stream = nil
}
