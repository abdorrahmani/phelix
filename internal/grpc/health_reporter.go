package grpc

import (
	"context"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/server"
)

// GrpcHealthReporter implements health.HealthReporter and sends auto-restart
// events to the backend via gRPC.
//
// It deliberately does NOT report health check results. Configuration and runtime
// status reach the backend as AppHealthSnapshot messages on the monitor stream
// (health_snapshot.go), where each message is a complete, idempotent statement of
// one app's health. An auto-restart is different in kind: a point-in-time fact
// that no later snapshot can reconstruct, so it keeps a dedicated RPC.
type GrpcHealthReporter struct {
	client *Client
}

// NewGrpcHealthReporter creates a reporter backed by the given gRPC client. The
// underlying PhelixServiceClient is resolved on every send rather than captured
// once: the Client replaces its connection on each reconnect, so holding on to
// a single stub would silently stop reporting after the first redial.
func NewGrpcHealthReporter(client *Client) *GrpcHealthReporter {
	return &GrpcHealthReporter{client: client}
}

// SendAutoRestartEvent sends an auto-restart event to the backend via gRPC.
//
// A disconnected client is not an error: the restart already happened locally and
// Phelix keeps working offline (the record is persisted to
// ~/.phelix/apps/<app>/health/restarts.json regardless). A live connection that
// rejects the event IS returned, classified by gRPC status, so the caller can log
// an authentication or validation failure as such instead of as "offline".
//
// An unwatched app is skipped the same way as a disconnected client: the restart
// already happened and is persisted locally, and an unwatched app must not
// generate backend monitoring events.
func (r *GrpcHealthReporter) SendAutoRestartEvent(record *health.AutoRestartRecord) error {
	if !app.IsWatched(record.AppID, record.AppName) {
		return nil
	}
	if r.client == nil {
		return nil
	}
	serviceClient := r.client.GetServiceClient()
	if !r.client.IsConnected() || serviceClient == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	authCtx, err := attachAuthMetadata(ctx, server.GetServerID())
	if err != nil {
		return err
	}

	resp, err := serviceClient.ReportAutoRestart(authCtx, &pb.ReportAutoRestartRequest{
		AppId:              record.AppID,
		AppName:            record.AppName,
		EndpointId:         record.EndpointID,
		EndpointName:       record.EndpointName,
		Reason:             record.Reason,
		ExitCode:           int32(record.ExitCode),
		BackoffNextSeconds: int32(record.BackoffNextSeconds),
		RestartedAt:        record.RestartedAt.UnixMilli(),
		CrashCount_24H:     int32(record.CrashCount24h),
	})
	if err != nil {
		return rpcFailed("report auto-restart", err)
	}
	if !resp.Success {
		return phelixerr.Newf(phelixerr.CodeServer, "backend rejected auto-restart event: %s", resp.GetError())
	}
	return nil
}
