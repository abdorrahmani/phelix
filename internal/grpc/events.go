package grpc

import (
	"context"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// SendEvent sends an application event to the backend via gRPC.
//
// Fire-and-forget variant kept for long-running callers (monitor daemon):
// failures only land in phelix.log. Interactive commands should use
// SendEventChecked (via ReportEventResult) so they can tell the user the
// dashboard is now stale.
func (c *Client) SendEvent(event *pb.ApplicationEvent) {
	_ = c.SendEventChecked(event)
}

// SendEventChecked sends an application event to the backend via gRPC and
// reports whether it was delivered. A nil return means the backend accepted
// the event; non-nil explains why it did not land (not connected, auth
// rejected, RPC error, or an EventResponse with accepted=false).
func (c *Client) SendEventChecked(event *pb.ApplicationEvent) error {
	if !c.IsConnected() {
		logs.ErrorFile("grpc", "[gRPC] Cannot send event '%s' for app '%s': not connected", event.GetAction(), event.GetAppName())
		return phelixerr.New(
			phelixerr.CodeConnection,
			"dashboard backend not connected",
		)
	}

	logs.InfoFile("grpc", "[gRPC] Sending event: action=%s app=%s app_id=%s success=%v agent_id=%s", event.GetAction(), event.GetAppName(), event.GetAppId(), event.GetSuccess(), server.GetAgentID())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to attach auth metadata: %v", err)
		return phelixerr.Wrap(phelixerr.CodeUnauthenticated, "no valid session for the dashboard backend", err)
	}

	resp, err := c.serviceClient.ReportEvent(authCtx, event)
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to send event: %v", err)
		// The backend's failed-auth budget (E2) is not a transient failure:
		// the attempt was rejected before validation. Report it as a
		// rate-limit so the operator sees the backoff instead of a generic
		// connection error, and do not force a reconnect.
		if authBudgetError(err) {
			return rateLimitedAuthError(err)
		}
		c.reconnectIfNeeded()
		rpcErr := phelixerr.FromGRPC(err)
		if code := phelixerr.CodeOf(rpcErr); code == phelixerr.CodeUnauthenticated || code == phelixerr.CodeSessionExpired {
			return phelixerr.Wrap(
				code,
				"the dashboard rejected this session; run 'phelix auth login' and retry",
				err,
			)
		}
		return phelixerr.Wrap(phelixerr.CodeGRPC, "failed to report event to the dashboard backend", err)
	}

	if !resp.Accepted {
		logs.ErrorFile("grpc", "[gRPC] Event rejected: %s", resp.Message)
		return phelixerr.Newf(
			phelixerr.CodeServer,
			"dashboard refused the event: %s",
			resp.Message,
		)
	}

	logs.InfoFile("grpc", "[gRPC] Event sent successfully: action=%s", event.GetAction())
	return nil
}

// NewApplicationEvent creates a new ApplicationEvent with the given parameters.
// The error message is redacted: caller-supplied err.Error() chains can embed
// lower-layer output (command stderr, environment dumps) and this event is
// serialized to the backend verbatim.
func NewApplicationEvent(appID, appName, action string, success bool, errMsg string, pid int, mode, version string) *pb.ApplicationEvent {
	return &pb.ApplicationEvent{
		ServerId:       server.GetServerID(),
		AppId:          appID,
		AppName:        appName,
		Action:         action,
		Timestamp:      time.Now().UnixMilli(),
		Success:        success,
		ErrorMessage:   phelixerr.Redact(errMsg),
		Pid:            int32(pid),
		DeploymentMode: mode,
		Version:        version,
	}
}

// SendAppEvent is a convenience method that creates and sends an event.
func (c *Client) SendAppEvent(appID, appName, action string, success bool, errMsg string, pid int, mode, version string) {
	event := NewApplicationEvent(appID, appName, action, success, errMsg, pid, mode, version)
	c.SendEvent(event)
}

// SendRollbackEvent sends a detailed rollback lifecycle event to the backend.
// Returns true if the event was sent and accepted, false otherwise.
func (c *Client) SendRollbackEvent(event *pb.RollbackLifecycleEvent) bool {
	if !c.IsConnected() {
		logs.ErrorFile("grpc", "[gRPC] Cannot send rollback event for app '%s': not connected", event.GetAppName())
		return false
	}

	logs.InfoFile("grpc", "[gRPC] Sending rollback event: step=%s app=%s success=%v server_id=%s cli_app_id=%s mode=%s strategy=%s current_ver=%s target_ver=%s target_tag=%s pid=%d versions=%d metadata=%d",
		event.GetCurrentStep(), event.GetAppName(), event.GetSuccess(),
		event.GetServerId(), event.GetCliAppId(),
		event.GetDeploymentMode(), event.GetRollbackStrategy(),
		event.GetCurrentVersion(), event.GetTargetVersion(), event.GetTargetTag(),
		event.GetPid(),
		len(event.GetVersions()), len(event.GetMetadata()),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to attach auth metadata for rollback event: %v", err)
		return false
	}

	resp, err := c.serviceClient.ReportRollbackEvent(authCtx, event)
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to send rollback event: %v", err)
		c.reconnectIfNeeded()
		return false
	}

	if !resp.Accepted {
		logs.ErrorFile("grpc", "[gRPC] Rollback event rejected: %s", resp.Message)
		return false
	}

	logs.InfoFile("grpc", "[gRPC] Rollback event sent: step=%s", event.GetCurrentStep())
	return true
}
