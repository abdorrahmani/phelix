package cmd

import (
	"testing"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func TestRemoteWorkerCommand_UnknownTypeIsError(t *testing.T) {
	got := RemoteWorkerCommand(&pb.MonitorCommandRequest{
		RequestId: "r1", Type: "worker_nope", AppName: "wk",
	})
	if got.GetStatus() != "error" || got.GetErrorCode() == "" {
		t.Fatalf("unknown worker command = %+v, want a coded error", got)
	}
}
