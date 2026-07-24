package grpc

import (
	"context"
	"io"
	"sync"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
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
func (c *Client) startAgentStream() {
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

		if err := c.openAgentStream(); err != nil {
			grpcLog("[gRPC Agent] Stream error: %v, reconnecting...", err)
			time.Sleep(2 * time.Second)
			continue
		}
	}
}

// openAgentStream opens a single AgentStream session and processes commands until it ends.
func (c *Client) openAgentStream() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		return err
	}

	stream, err := c.serviceClient.AgentStream(authCtx)
	if err != nil {
		return err
	}

	agentStream.mu.Lock()
	agentStream.stream = stream
	agentStream.cancel = cancel
	agentStream.mu.Unlock()

	grpcLog("[gRPC Agent] AgentStream connected")

	// Send initial pong to announce presence
	if err := stream.Send(&pb.ClientToServer{
		Payload: &pb.ClientToServer_Pong{
			Pong: &pb.Pong{
				Id:        serverID,
				Timestamp: time.Now().UnixMilli(),
			},
		},
	}); err != nil {
		return err
	}

	// Process incoming commands
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			grpcLog("[gRPC Agent] Stream closed by backend")
			return nil
		}
		if err != nil {
			return err
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
	grpcLog("[gRPC Agent] Health command: request_id=%s", cmd.GetRequestId())

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
		grpcLog("[gRPC Agent] Failed to send health result: %v", err)
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
			Error:     err.Error(),
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
				grpcLog("[gRPC Agent] Failed to send health update: %v", err)
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
		grpcLog("[gRPC Agent] Cancelling request: %s", cancel.GetRequestId())
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
		grpcLog("[gRPC Agent] Failed to send pong: %v", err)
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
