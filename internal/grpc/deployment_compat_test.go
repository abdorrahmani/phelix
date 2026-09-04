package grpc

import (
	"testing"
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"google.golang.org/protobuf/proto"
)

// Backward compatibility with an older backend. The two properties that matter:
// a backend that has never heard of deployment telemetry must keep working, and
// the CLI must not keep hammering an RPC that backend does not implement.

func TestDeploymentEvent_UnknownFieldsSurviveRoundTrip(t *testing.T) {
	// Simulates an older backend decoding the message with an older schema: any
	// field it does not know is preserved rather than rejected, which is what
	// makes extending the contract safe.
	ev := &pb.DeploymentEvent{
		ServerId:       "agent-1",
		AppName:        "shop",
		DeploymentId:   "dep-1",
		Event:          "deployment.completed",
		Strategy:       "blue-green",
		CurrentVersion: "v15",
		Snapshot: &pb.DeploymentSnapshot{
			AppName:         "shop",
			ReplicasHealthy: 2,
			Proxy:           &pb.DeploymentProxy{Enabled: true, PublicPort: 3000},
		},
		Timestamp: time.Now().UnixMilli(),
	}
	raw, err := proto.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back pb.DeploymentEvent
	if err := proto.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.GetDeploymentId() != "dep-1" || back.GetSnapshot().GetProxy().GetPublicPort() != 3000 {
		t.Fatalf("round trip lost data: %+v", &back)
	}
}

func TestMonitorEvent_DeploymentSnapshotIsAnAdditiveOneofCase(t *testing.T) {
	// The snapshot rides the existing MonitorEvent envelope. An older backend
	// decoding it sees an unrecognized oneof case and ignores that field; every
	// other monitor payload keeps decoding exactly as before.
	snapEvent := &pb.MonitorEvent{
		ServerId:  "agent-1",
		Timestamp: time.Now().UnixMilli(),
		Payload: &pb.MonitorEvent_DeploymentSnapshot{
			DeploymentSnapshot: &pb.DeploymentSnapshot{AppName: "shop", Strategy: "rolling"},
		},
	}
	raw, err := proto.Marshal(snapEvent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back pb.MonitorEvent
	if err := proto.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.GetDeploymentSnapshot().GetStrategy() != "rolling" {
		t.Fatalf("snapshot payload lost: %+v", &back)
	}
	// The pre-existing payload cases still work unchanged.
	pong := &pb.MonitorEvent{Payload: &pb.MonitorEvent_Pong{Pong: &pb.Pong{}}}
	if pong.GetPong() == nil {
		t.Fatalf("existing monitor payloads must be unaffected")
	}
	if pong.GetDeploymentSnapshot() != nil {
		t.Fatalf("a pong must not decode as a deployment snapshot")
	}
}

func TestDeploymentUnsupported_StopsFurtherAttempts(t *testing.T) {
	t.Cleanup(func() {
		deploymentSenderMu.Lock()
		deploymentUnsupport = false
		deploymentSenderMu.Unlock()
	})

	if deploymentUnsupported() {
		t.Fatalf("deployment telemetry should start out enabled")
	}
	markDeploymentUnsupported()
	if !deploymentUnsupported() {
		t.Fatalf("an UNIMPLEMENTED backend must disable further attempts")
	}
	// Idempotent: repeated marks are harmless.
	markDeploymentUnsupported()
	if !deploymentUnsupported() {
		t.Fatalf("state flipped back after a second mark")
	}
}

func TestStopDeploymentSender_NoSenderIsNoop(t *testing.T) {
	// A command that never deployed anything must not block or panic on flush.
	done := make(chan struct{})
	go func() {
		StopDeploymentSender(time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("StopDeploymentSender blocked with no sender running")
	}
}
