package grpc

import (
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/server"
)

// Conversion from the deploy package's telemetry types to the wire messages.
// Kept separate from transport so the mapping is testable without a connection.

// toProtoDeploymentEvent converts one lifecycle event, attaching the reporting
// runtime's identity and the operator's session id. The raw session token is
// deliberately absent from the body — the RPC is authenticated via gRPC
// metadata, and a second copy of a credential inside a serialized message
// would be a credential in transit that nothing needs.
func toProtoDeploymentEvent(ev deploy.Event) *pb.DeploymentEvent {
	_ = server.Initialize()
	out := &pb.DeploymentEvent{
		ServerId:        server.GetServerID(),
		AppId:           ev.AppID,
		AppName:         ev.AppName,
		DeploymentId:    ev.DeploymentID,
		RequestId:       ev.RequestID,
		Event:           ev.Event,
		Strategy:        ev.Strategy,
		Phase:           ev.Phase,
		Status:          ev.Status,
		CurrentVersion:  ev.CurrentVersion,
		TargetVersion:   ev.TargetVersion,
		Slot:            ev.Slot,
		ReplicaId:       ev.ReplicaID,
		ReplicaIndex:    int32(ev.ReplicaIndex),
		InternalPort:    int32(ev.InternalPort),
		Pid:             int32(ev.PID),
		ReplicasDesired: int32(ev.ReplicasDesired),
		Message:         sanitizeUTF8(ev.Message),
		Failure:         toProtoDeploymentFailure(ev.Failure),
		Snapshot:        ToProtoDeploymentSnapshot(ev.Snapshot),
		Timestamp:       unixMilli(ev.Timestamp),
	}
	if sid, _ := loadSessionIdentity(); sid != "" {
		out.UserId = sid
	}
	// Replica events carry the replica index in Slot for internal keying; the
	// wire contract reserves slot for blue-green slot names.
	if out.ReplicaId != "" {
		out.Slot = ""
	}
	return out
}

// ToProtoDeploymentSnapshot converts a deployment snapshot. Exported so the
// monitor stream can push a resync snapshot built by deploy.SnapshotForApp.
func ToProtoDeploymentSnapshot(s *deploy.Snapshot) *pb.DeploymentSnapshot {
	if s == nil {
		return nil
	}
	out := &pb.DeploymentSnapshot{
		ServerId:        server.GetServerID(),
		AppId:           s.AppID,
		AppName:         s.AppName,
		DeploymentId:    s.DeploymentID,
		RequestId:       s.RequestID,
		Strategy:        s.Strategy,
		Phase:           s.Phase,
		Status:          s.Status,
		CurrentVersion:  s.CurrentVersion,
		TargetVersion:   s.TargetVersion,
		ActiveSlot:      s.ActiveSlot,
		ReplicasDesired: int32(s.ReplicasDesired),
		ReplicasCurrent: int32(s.ReplicasCurrent),
		ReplicasReady:   int32(s.ReplicasReady),
		ReplicasHealthy: int32(s.ReplicasHealthy),
		Failure:         toProtoDeploymentFailure(s.Failure),
		StartedAt:       unixMilli(s.StartedAt),
		UpdatedAt:       unixMilli(s.UpdatedAt),
		Runtime:         s.Runtime,
	}
	for _, sl := range s.Slots {
		out.Slots = append(out.Slots, &pb.DeploymentSlot{
			Slot:         sl.Slot,
			Version:      sl.Version,
			Status:       sl.Status,
			Health:       sl.Health,
			InternalPort: int32(sl.InternalPort),
			Pid:          int32(sl.PID),
			Active:       sl.Active,
			StartedAt:    unixMilli(sl.StartedAt),
			HealthyAt:    unixMilli(sl.HealthyAt),
			Image:        sl.Image,
			ContainerId:  sl.ContainerID,
		})
	}
	for _, rp := range s.Replicas {
		out.Replicas = append(out.Replicas, &pb.DeploymentReplica{
			Id:           rp.ID,
			Index:        int32(rp.Index),
			Version:      rp.Version,
			Status:       rp.Status,
			Health:       rp.Health,
			InternalPort: int32(rp.InternalPort),
			Pid:          int32(rp.PID),
			StartedAt:    unixMilli(rp.StartedAt),
			HealthyAt:    unixMilli(rp.HealthyAt),
			Image:        rp.Image,
			ContainerId:  rp.ContainerID,
		})
	}
	if p := s.Proxy; p != nil {
		out.Proxy = &pb.DeploymentProxy{
			Enabled:            p.Enabled,
			PublicPort:         int32(p.PublicPort),
			TargetSlot:         p.TargetLabel,
			TargetInternalPort: int32(p.TargetInternalPort),
			Upstreams:          append([]string(nil), p.Upstreams...),
			InFlight:           p.InFlight,
		}
	}
	if h := s.Health; h != nil {
		out.Health = &pb.DeploymentHealth{
			Mode:          h.Mode,
			Path:          h.Path,
			Retries:       int32(h.Retries),
			Interval:      h.Interval,
			Timeout:       h.Timeout,
			Tier:          int32(h.Tier),
			TierLabel:     h.TierLabel,
			LastHealthyAt: unixMilli(h.LastHealthyAt),
		}
	}
	return out
}

func toProtoDeploymentFailure(f *deploy.Failure) *pb.DeploymentFailure {
	if f == nil {
		return nil
	}
	return &pb.DeploymentFailure{
		Code: f.Code,
		// A failure message quotes tool/instance output, which can carry
		// arbitrary bytes. Invalid UTF-8 makes the backend reject the whole
		// request ("string field contains invalid UTF-8"), so it is repaired
		// here for the same reason the rollback reporter does it.
		Message:   sanitizeUTF8(f.Message),
		Retryable: f.Retryable,
	}
}

// unixMilli maps a zero time to 0 rather than to the Unix epoch in
// milliseconds, so "unknown" stays distinguishable from "1970".
func unixMilli(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
