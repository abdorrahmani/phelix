package grpc

import (
	"context"
	"time"

	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/connstate"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// handleAuthRejected reacts to the backend rejecting our credentials
// (UNAUTHENTICATED / expired or revoked session). It stops authenticated
// synchronization without touching the local runtime: managed apps keep
// running and local lifecycle commands keep working. The reconnect loop stops
// redialing with the invalid credential; 'phelix auth login' restores the
// connection using the same persistent agent_id.
func (c *Client) handleAuthRejected() {
	connstate.MarkAuthExpired()
	c.Close()
	logs.WarningFile("grpc", "[gRPC] Backend rejected credentials (auth_expired); dashboard sync paused until 'phelix auth login'. Local runtime and apps are unaffected.")
}

// NotifyLogout explicitly tells the backend this agent is logging out so it
// can mark the agent disconnected immediately, instead of waiting for a
// heartbeat timeout. The backend must NOT delete the agent record — the call
// only updates connection_state keyed by the persistent agent_id.
//
// reason is connstate.Disconnected for an explicit 'phelix auth logout' or
// connstate.AuthExpired when credentials were revoked. Idempotent: calling
// twice is harmless (the backend just rewrites the same state).
//
// best-effort by design:
//   - No session (logged out already) → nothing to notify, returns nil.
//   - No gRPC URL configured → nothing to notify, returns nil.
//   - Network failure → the caller still deletes the local session; the
//     backend falls back to heartbeat-timeout detection.
func NotifyLogout(reason string) error {
	cfg := config.Get()
	if cfg == nil || cfg.App.GRPCUrl == "" {
		return nil // nowhere to notify
	}
	if !sessionAvailable() {
		return nil // no active backend session; idempotent no-op
	}
	if err := server.Initialize(); err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to initialize server for logout notification: %v", err)
		return nil
	}

	c := GetClient()
	temporary := false
	if c == nil || !c.IsConnected() {
		// Monitor daemon not running (or not connected): dial briefly just for
		// the logout notification.
		c = NewClient()
		temporary = true
		if err := c.Connect(); err != nil {
			logs.WarningFile("grpc", "[gRPC] Could not connect to notify logout: %v", err)
			return nil
		}
		defer func() {
			if temporary {
				c.Close()
			}
		}()
	}

	client := c.GetServiceClient()
	if client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	authCtx, err := attachAuthMetadata(ctx, server.GetServerID())
	if err != nil {
		return nil // session vanished mid-logout; still fine
	}

	resp, err := client.AgentLogout(authCtx, &pb.AgentLogoutRequest{
		AgentId: server.GetAgentID(),
		Reason:  reason,
	})
	if err != nil {
		logs.WarningFile("grpc", "[gRPC] Logout notification failed: %v", phelixerr.FromGRPC(err))
		return nil
	}
	logs.InfoFile("grpc", "[gRPC] Backend notified of logout (agent_id=%s accepted=%v)", server.GetAgentID(), resp.GetAccepted())
	return nil
}
