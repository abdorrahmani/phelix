package grpc

import (
	"context"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
)

// rpcFailed converts a gRPC RPC failure into a structured Phelix error. The
// code is derived from the gRPC status code so Unauthenticated, PermissionDenied,
// Unavailable and DeadlineExceeded remain distinguishable — not every RPC
// failure is a connection failure. The original status error is preserved as
// the cause so status.Code / errors.Is still inspect it through the wrap.
func rpcFailed(operation string, err error) error {
	if err == nil {
		return nil
	}
	code := phelixerr.CodeConnection
	if mapped := phelixerr.FromGRPC(err); mapped != nil {
		if e := phelixerr.AsError(mapped); e != nil {
			code = e.Code
		}
	}
	if code == phelixerr.CodeUnknown {
		code = phelixerr.CodeConnection
	}
	return phelixerr.Wrapf(code, err, "failed to %s", operation)
}

// sendHealthSetConfigWithClient sends a HealthSetConfigRequest using the given client.
func sendHealthSetConfigWithClient(c *Client, req *pb.HealthSetConfigRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		return err
	}

	resp, err := c.serviceClient.HealthSetConfig(authCtx, req)
	if err != nil {
		return rpcFailed("send health configuration", err)
	}
	if !resp.Success {
		logs.Error("grpc", "[gRPC] HealthSetConfig rejected: %s", resp.GetError())
	}
	return nil
}

// sendHealthAddEndpointWithClient sends a HealthAddEndpointRequest using the given client.
func sendHealthAddEndpointWithClient(c *Client, req *pb.HealthAddEndpointRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		return err
	}

	resp, err := c.serviceClient.HealthAddEndpoint(authCtx, req)
	if err != nil {
		return rpcFailed("add health endpoint", err)
	}
	if !resp.Success {
		logs.Error("grpc", "[gRPC] HealthAddEndpoint rejected: %s", resp.GetError())
	}
	return nil
}

// sendHealthRemoveEndpointWithClient sends a HealthRemoveEndpointRequest using the given client.
func sendHealthRemoveEndpointWithClient(c *Client, req *pb.HealthRemoveEndpointRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverID := server.GetServerID()
	authCtx, err := attachAuthMetadata(ctx, serverID)
	if err != nil {
		return err
	}

	resp, err := c.serviceClient.HealthRemoveEndpoint(authCtx, req)
	if err != nil {
		return rpcFailed("remove health endpoint", err)
	}
	if !resp.Success {
		logs.Error("grpc", "[gRPC] HealthRemoveEndpoint rejected: %s", resp.GetError())
	}
	return nil
}

// SendHealthSetConfig sends health set config to the backend via gRPC.
// Tries the global client first; falls back to a temporary connection.
func SendHealthSetConfig(appID, appName, path, interval, timeout, expectedCodes, mode string, retries int) {
	if err := server.Initialize(); err != nil {
		logs.Error("grpc", "[gRPC] Failed to initialize server for health set: %v", err)
		return
	}

	req := &pb.HealthSetConfigRequest{
		AppId:         appID,
		AppName:       appName,
		Path:          path,
		Interval:      interval,
		Retries:       int32(retries),
		ExpectedCodes: expectedCodes,
		Timeout:       timeout,
		Mode:          mode,
	}

	if c := GetClient(); c != nil && c.IsConnected() {
		if err := sendHealthSetConfigWithClient(c, req); err != nil {
			logs.Error("grpc", "[gRPC] HealthSetConfig failed: %v", err)
		}
		return
	}

	logs.Warning("grpc", "[gRPC] No global client, creating temporary connection for HealthSetConfig")
	c := NewClient()
	if err := c.Connect(); err != nil {
		logs.Error("grpc", "[gRPC] Failed to create temporary connection: %v", err)
		return
	}
	defer c.Close()

	if err := sendHealthSetConfigWithClient(c, req); err != nil {
		logs.Error("grpc", "[gRPC] HealthSetConfig failed: %v", err)
	}
}

// SendHealthAddEndpoint sends health add endpoint to the backend via gRPC.
// Tries the global client first; falls back to a temporary connection.
func SendHealthAddEndpoint(appID, appName string, config *health.HealthCheckConfig) {
	if err := server.Initialize(); err != nil {
		logs.Error("grpc", "[gRPC] Failed to initialize server for health add: %v", err)
		return
	}

	req := &pb.HealthAddEndpointRequest{
		AppId:         appID,
		AppName:       appName,
		Name:          config.Name,
		Url:           config.URL,
		Interval:      config.Interval,
		Retries:       int32(config.Retries),
		ExpectedCodes: config.ExpectedCodes,
		Timeout:       config.Timeout,
	}

	if c := GetClient(); c != nil && c.IsConnected() {
		if err := sendHealthAddEndpointWithClient(c, req); err != nil {
			logs.Error("grpc", "[gRPC] HealthAddEndpoint failed: %v", err)
		}
		return
	}

	logs.Warning("grpc", "[gRPC] No global client, creating temporary connection for HealthAddEndpoint")
	c := NewClient()
	if err := c.Connect(); err != nil {
		logs.Error("grpc", "[gRPC] Failed to create temporary connection: %v", err)
		return
	}
	defer c.Close()

	if err := sendHealthAddEndpointWithClient(c, req); err != nil {
		logs.Error("grpc", "[gRPC] HealthAddEndpoint failed: %v", err)
	}
}

// SendHealthRemoveEndpoint sends health remove endpoint to the backend via gRPC.
// Tries the global client first; falls back to a temporary connection.
func SendHealthRemoveEndpoint(appID, appName, endpointName string) {
	if err := server.Initialize(); err != nil {
		logs.Error("grpc", "[gRPC] Failed to initialize server for health remove: %v", err)
		return
	}

	req := &pb.HealthRemoveEndpointRequest{
		AppId:   appID,
		AppName: appName,
		Name:    endpointName,
	}

	if c := GetClient(); c != nil && c.IsConnected() {
		if err := sendHealthRemoveEndpointWithClient(c, req); err != nil {
			logs.Error("grpc", "[gRPC] HealthRemoveEndpoint failed: %v", err)
		}
		return
	}

	logs.Warning("grpc", "[gRPC] No global client, creating temporary connection for HealthRemoveEndpoint")
	c := NewClient()
	if err := c.Connect(); err != nil {
		logs.Error("grpc", "[gRPC] Failed to create temporary connection: %v", err)
		return
	}
	defer c.Close()

	if err := sendHealthRemoveEndpointWithClient(c, req); err != nil {
		logs.Error("grpc", "[gRPC] HealthRemoveEndpoint failed: %v", err)
	}
}
