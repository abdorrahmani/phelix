package grpc

import (
	"time"

	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/google/uuid"
)

const SecurityProtocolVersion = 2
const agentHeartbeatInterval = 10 * time.Second

func newAgentStatus() *pb.AgentStatus {
	return &pb.AgentStatus{
		ConnectionId:            uuid.NewString(),
		SecurityProtocolVersion: SecurityProtocolVersion,
		Capabilities:            AgentCapabilities(),
		AgentVersion:            version.Version,
		SessionScope:            PreferredSessionScope(),
		HeartbeatIntervalMs:     agentHeartbeatInterval.Milliseconds(),
	}
}

func sendAgentStatus(status *pb.AgentStatus) error {
	return monitorStream.send(&pb.MonitorEvent{
		ServerId: server.GetAgentID(), Timestamp: time.Now().UnixMilli(),
		Payload: &pb.MonitorEvent_AgentStatus{AgentStatus: status},
	})
}
