package grpc

import (
	"context"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
)

// SendEvent sends an application event to the backend via gRPC.
func (c *Client) SendEvent(event *pb.ApplicationEvent) {
	if !c.IsConnected() {
		grpcLog("[gRPC] Cannot send event '%s' for app '%s': not connected", event.GetAction(), event.GetAppName())
		return
	}

	grpcLog("[gRPC] Sending event: action=%s app=%s success=%v", event.GetAction(), event.GetAppName(), event.GetSuccess())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		grpcLog("[gRPC] Failed to attach auth metadata: %v", err)
		return
	}

	resp, err := c.serviceClient.ReportEvent(authCtx, event)
	if err != nil {
		grpcLog("[gRPC] Failed to send event: %v", err)
		c.reconnectIfNeeded()
		return
	}

	if !resp.Accepted {
		grpcLog("[gRPC] Event rejected: %s", resp.Message)
	} else {
		grpcLog("[gRPC] Event sent successfully: action=%s", event.GetAction())
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
