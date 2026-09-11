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
	mu            sync.RWMutex
	stream        pb.PhelixService_AgentStreamClient
	cancel        context.CancelFunc
	activeWatches map[string]context.CancelFunc // request_id -> cancel
}

var agentStream = &agentStreamManager{
	activeWatches: make(map[string]context.CancelFunc),
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
	if err := stream.Send(&pb.ClientToServer{
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

	// Handle watch commands via streaming
	if watchCmd := cmd.GetWatch(); watchCmd != nil {
		c.handleHealthWatch(stream, cmd, watchCmd)
		return
	}

	// Process regular commands synchronously
	result := ProcessHealthCommand(cmd)

	if err := stream.Send(&pb.ClientToServer{
		Payload: &pb.ClientToServer_HealthResult{
			HealthResult: result,
		},
	}); err != nil {
		logs.ErrorFile("grpc", "[gRPC Agent] Failed to send health result: %v", err)
	}
}

// handleHealthWatch processes a health watch command and streams updates.
func (c *Client) handleHealthWatch(stream pb.PhelixService_AgentStreamClient, cmd *pb.HealthCommand, watchCmd *pb.HealthWatchCommand) {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// Store cancel function for this watch
	agentStream.mu.Lock()
	agentStream.activeWatches[cmd.GetRequestId()] = cancel
	agentStream.mu.Unlock()

	defer func() {
		agentStream.mu.Lock()
		delete(agentStream.activeWatches, cmd.GetRequestId())
		agentStream.mu.Unlock()
	}()

	updateChan, err := ProcessHealthWatch(watchCmd.GetAppId(), watchCmd.GetAppName())
	if err != nil {
		result := &pb.HealthResult{
			RequestId: cmd.GetRequestId(),
			ServerId:  cmd.GetServerId(),
			Command:   "health_watch",
			Success:   false,
			Timestamp: time.Now().UnixMilli(),
			Error:     phelixerr.Redact(err.Error()),
		}
		stream.Send(&pb.ClientToServer{
			Payload: &pb.ClientToServer_HealthResult{
				HealthResult: result,
			},
		})
		return
	}

	// Send initial status
	initialResult := ProcessHealthCommand(cmd)
	stream.Send(&pb.ClientToServer{
		Payload: &pb.ClientToServer_HealthResult{
			HealthResult: initialResult,
		},
	})

	// Stream updates
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
			if err := stream.Send(&pb.ClientToServer{
				Payload: &pb.ClientToServer_HealthUpdate{
					HealthUpdate: update,
				},
			}); err != nil {
				logs.ErrorFile("grpc", "[gRPC Agent] Failed to send health update: %v", err)
				return
			}
		}
	}
}

// handleCancelRequest cancels an ongoing operation.
func (c *Client) handleCancelRequest(cancel *pb.CancelRequest) {
	agentStream.mu.Lock()
	defer agentStream.mu.Unlock()

	if cancelFn, ok := agentStream.activeWatches[cancel.GetRequestId()]; ok {
		logs.InfoFile("grpc", "[gRPC Agent] Cancelling request: %s", cancel.GetRequestId())
		cancelFn()
		delete(agentStream.activeWatches, cancel.GetRequestId())
	}
}

// handlePing responds to a keepalive ping.
func (c *Client) handlePing(stream pb.PhelixService_AgentStreamClient, ping *pb.Ping) {
	if err := stream.Send(&pb.ClientToServer{
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
	for id, cancel := range agentStream.activeWatches {
		cancel()
		delete(agentStream.activeWatches, id)
	}

	// Cancel the stream context
	if agentStream.cancel != nil {
		agentStream.cancel()
	}

	agentStream.stream = nil
}
