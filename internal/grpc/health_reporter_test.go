package grpc

import (
	"context"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// autoRestartBackend records the auto-restart event it received and answers with
// a configurable verdict.
type autoRestartBackend struct {
	pb.UnimplementedPhelixServiceServer

	got     *pb.ReportAutoRestartRequest
	success bool
	rpcErr  error
}

func (b *autoRestartBackend) ReportAutoRestart(_ context.Context, req *pb.ReportAutoRestartRequest) (*pb.ReportAutoRestartResponse, error) {
	b.got = req
	if b.rpcErr != nil {
		return nil, b.rpcErr
	}
	return &pb.ReportAutoRestartResponse{Success: b.success}, nil
}

func testRestartRecord() *health.AutoRestartRecord {
	return &health.AutoRestartRecord{
		AppID:              testAppID,
		AppName:            "billing",
		EndpointID:         "ep-1",
		EndpointName:       "default",
		Reason:             "3 consecutive health check failures on default",
		ExitCode:           0,
		BackoffNextSeconds: 5,
		RestartedAt:        time.Now(),
		CrashCount24h:      2,
	}
}

// The event must carry the endpoint identity, so the backend can attribute the
// restart without parsing the human-readable reason string.
func TestSendAutoRestartEvent_CarriesEndpointIdentity(t *testing.T) {
	setupTestSession(t)

	backend := &autoRestartBackend{success: true}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	if err := NewGrpcHealthReporter(c).SendAutoRestartEvent(testRestartRecord()); err != nil {
		t.Fatalf("SendAutoRestartEvent: %v", err)
	}
	if backend.got == nil {
		t.Fatal("backend received no auto-restart event")
	}
	if backend.got.GetEndpointId() != "ep-1" || backend.got.GetEndpointName() != "default" {
		t.Fatalf("endpoint identity not carried: id=%q name=%q",
			backend.got.GetEndpointId(), backend.got.GetEndpointName())
	}
	if backend.got.GetAppId() != testAppID || backend.got.GetCrashCount_24H() != 2 {
		t.Fatalf("unexpected payload: %+v", backend.got)
	}
}

// A backend that rejects the event is an error the caller can log as such — not
// silently discarded, which is how the old path handled every rejection.
func TestSendAutoRestartEvent_RejectionIsAnError(t *testing.T) {
	setupTestSession(t)

	backend := &autoRestartBackend{success: false}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	err := NewGrpcHealthReporter(c).SendAutoRestartEvent(testRestartRecord())
	if err == nil {
		t.Fatal("expected an error when the backend rejects the event")
	}
	if got := phelixerr.CodeOf(err); got != phelixerr.CodeServer {
		t.Fatalf("error code = %s, want %s", got, phelixerr.CodeServer)
	}
}

// An RPC failure must stay classified by its gRPC status, so an expired token is
// distinguishable from an unreachable backend.
func TestSendAutoRestartEvent_ClassifiesRPCFailure(t *testing.T) {
	setupTestSession(t)

	backend := &autoRestartBackend{rpcErr: status.Error(codes.Unauthenticated, "token expired")}
	c, _, cleanup := dialBufconn(t, backend)
	defer cleanup()

	err := NewGrpcHealthReporter(c).SendAutoRestartEvent(testRestartRecord())
	if err == nil {
		t.Fatal("expected an error when the RPC fails")
	}
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("lost the gRPC status: got %s", got)
	}
}

// Offline is not a synchronization error: the restart already happened locally
// and the record is persisted on disk regardless, so Phelix keeps working.
func TestSendAutoRestartEvent_DisconnectedIsNotAnError(t *testing.T) {
	if err := NewGrpcHealthReporter(nil).SendAutoRestartEvent(testRestartRecord()); err != nil {
		t.Fatalf("nil client must be a no-op, got %v", err)
	}
	if err := NewGrpcHealthReporter(&Client{}).SendAutoRestartEvent(testRestartRecord()); err != nil {
		t.Fatalf("disconnected client must be a no-op, got %v", err)
	}
}
