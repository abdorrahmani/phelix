package health

import (
	"context"
	"log"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

// GrpcHealthReporter implements WebSocketClient and sends health data via gRPC.
// Used by the daemon to report results to the backend.
type GrpcHealthReporter struct {
	client pb.PhelixServiceClient
}

// NewGrpcHealthReporter creates a reporter with the given gRPC client.
func NewGrpcHealthReporter(client pb.PhelixServiceClient) *GrpcHealthReporter {
	return &GrpcHealthReporter{client: client}
}

// SendHealthCheckResult sends a health check result to the backend via gRPC.
func (r *GrpcHealthReporter) SendHealthCheckResult(result *HealthCheckResult, appID string, appName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var statusCode int32
	if result.StatusCode != nil {
		statusCode = int32(*result.StatusCode)
	}
	var latencyMs int64
	if result.LatencyMs != nil {
		latencyMs = *result.LatencyMs
	}
	var errMsg string
	if result.Error != nil {
		errMsg = *result.Error
	}

	resp, err := r.client.ReportHealthResult(ctx, &pb.ReportHealthResultRequest{
		AppId:        appID,
		AppName:      appName,
		EndpointName: result.EndpointName,
		Url:          result.URL,
		Status:       result.Status,
		StatusCode:   statusCode,
		LatencyMs:    latencyMs,
		CheckedAt:    result.CheckedAt.UnixMilli(),
		Error:        errMsg,
	})
	if err != nil {
		log.Printf("[Health gRPC] Failed to report health result: %v", err)
		return err
	}
	if !resp.Success {
		log.Printf("[Health gRPC] Health result rejected: %s", resp.GetError())
	}
	return nil
}

// SendAutoRestartEvent sends an auto-restart event to the backend via gRPC.
func (r *GrpcHealthReporter) SendAutoRestartEvent(record *AutoRestartRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := r.client.ReportAutoRestart(ctx, &pb.ReportAutoRestartRequest{
		AppId:              record.AppID,
		AppName:            record.AppName,
		Reason:             record.Reason,
		ExitCode:           int32(record.ExitCode),
		BackoffNextSeconds: int32(record.BackoffNextSeconds),
		RestartedAt:        record.RestartedAt.UnixMilli(),
		CrashCount_24H:     int32(record.CrashCount24h),
	})
	if err != nil {
		log.Printf("[Health gRPC] Failed to report auto-restart: %v", err)
		return err
	}
	if !resp.Success {
		log.Printf("[Health gRPC] Auto-restart report rejected: %s", resp.GetError())
	}
	return nil
}

// IsConnected returns whether the gRPC client is available.
func (r *GrpcHealthReporter) IsConnected() bool {
	return r.client != nil
}
