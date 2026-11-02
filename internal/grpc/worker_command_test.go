package grpc

import (
	"testing"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
)

func TestIsWorkerCommand_CoversEveryWorkerType(t *testing.T) {
	for _, cmd := range []string{WorkerCommandScale, WorkerCommandRestartReplica} {
		if !isWorkerCommand(cmd) {
			t.Fatalf("%s must dispatch as a worker command", cmd)
		}
	}
	if isWorkerCommand("restart") {
		t.Fatal("generic restart must not dispatch through the worker ledger")
	}
}

func TestValidateWorkerCommand_ScaleShape(t *testing.T) {
	base := &pb.MonitorCommandRequest{RequestId: "r1", Type: WorkerCommandScale, AppName: "wk"}
	ok := *base
	ok.Replicas = 3
	if err := validateWorkerCommand(&ok); err != nil {
		t.Fatalf("valid scale rejected: %v", err)
	}

	noReplicas := *base
	if err := validateWorkerCommand(&noReplicas); err == nil {
		t.Fatal("scale without replicas must be rejected")
	}

	withReplica := *base
	withReplica.Replicas = 2
	withReplica.Replica = "0"
	if err := validateWorkerCommand(&withReplica); err == nil {
		t.Fatal("scale carrying a replica must be rejected")
	}
}

func TestValidateWorkerCommand_RestartReplicaShape(t *testing.T) {
	base := &pb.MonitorCommandRequest{RequestId: "r2", Type: WorkerCommandRestartReplica, AppName: "wk"}
	ok := *base
	ok.Replica = "1"
	if err := validateWorkerCommand(&ok); err != nil {
		t.Fatalf("valid replica restart rejected: %v", err)
	}

	noReplica := *base
	if err := validateWorkerCommand(&noReplica); err == nil {
		t.Fatal("restart without a replica must be rejected")
	}

	withCount := *base
	withCount.Replica = "1"
	withCount.Replicas = 2
	if err := validateWorkerCommand(&withCount); err == nil {
		t.Fatal("replica restart carrying a replica count must be rejected")
	}
}

func TestValidateWorkerCommand_UnknownTypeRejected(t *testing.T) {
	err := validateWorkerCommand(&pb.MonitorCommandRequest{
		RequestId: "r3", Type: "worker_nope", AppName: "wk",
	})
	if err == nil {
		t.Fatal("unknown worker command type must be rejected")
	}
}
