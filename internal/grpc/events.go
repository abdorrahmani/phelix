package grpc

import (
	"context"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// SendEvent sends an application event to the backend via gRPC.
func (c *Client) SendEvent(event *pb.ApplicationEvent) {
	if !c.IsConnected() {
		logs.ErrorFile("grpc", "[gRPC] Cannot send event '%s' for app '%s': not connected", event.GetAction(), event.GetAppName())
		return
	}

	logs.InfoFile("grpc", "[gRPC] Sending event: action=%s app=%s success=%v", event.GetAction(), event.GetAppName(), event.GetSuccess())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to attach auth metadata: %v", err)
		return
	}

	resp, err := c.serviceClient.ReportEvent(authCtx, event)
	if err != nil {
		logs.ErrorFile("grpc", "[gRPC] Failed to send event: %v", err)
		c.reconnectIfNeeded()
		return
	}

	if !resp.Accepted {
		logs.ErrorFile("grpc", "[gRPC] Event rejected: %s", resp.Message)
	} else {
		logs.InfoFile("grpc", "[gRPC] Event sent successfully: action=%s", event.GetAction())
	}
}

// NewApplicationEvent creates a new ApplicationEvent with the given parameters.
func NewApplicationEvent(appID, appName, action string, success bool, errMsg string, pid int, mode, version string) *pb.ApplicationEvent {
	return &pb.ApplicationEvent{
		ServerId:       server.GetServerID(),
		AppId:          appID,
		AppName:        appName,
		Action:         action,
		Timestamp:      time.Now().UnixMilli(),
		Success:        success,
		ErrorMessage:   errMsg,
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
