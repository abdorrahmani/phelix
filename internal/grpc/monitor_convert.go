package grpc

import (
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/server"
)

// toProtoServerInfo converts server.Info to its protobuf representation.
// Mirrors the old "servers" WebSocket message payload.
func toProtoServerInfo(info *server.Info) *pb.ServerInfo {
	if info == nil {
		return nil
	}
	return &pb.ServerInfo{
		Id:            info.ID,
		Hostname:      info.Hostname,
		IpV4:          info.IPv4,
		IpV6:          info.IPv6,
		OsType:        info.OSType,
		OsFull:        info.OSFull,
		CpuInfo:       info.CPUInfo,
		NetworkIfaces: info.NetworkIfaces,
		TotalMemory:   info.TotalMemory,
		TotalCpuCores: int32(info.TotalCPUCores),
		TotalStorage:  info.TotalStorage,
		Uptime:        info.Uptime,
		LastReboot:    info.LastReboot.UnixMilli(),
		Status:        info.Status,
		CreatedAt:     info.CreatedAt.UnixMilli(),
		Region:        info.Region,
		Architecture:  info.Architecture,
		KernelVersion: info.KernelVersion,
		SwapTotal:     info.SwapTotal,
	}
}

// toProtoServerMetrics converts server.Metrics to its protobuf
// representation. Mirrors the old "server_metrics" WebSocket message.
func toProtoServerMetrics(m *server.Metrics) *pb.ServerMetrics {
	if m == nil {
		return nil
	}
	return &pb.ServerMetrics{
		ServerId:         m.ServerID,
		UsedMemory:       m.UsedMemory,
		FreeMemory:       m.FreeMemory,
		MemoryPercent:    m.MemoryPercent,
		CpuUsagePercent:  m.CPUUsagePercent,
		UsedStorage:      m.UsedStorage,
		FreeStorage:      m.FreeStorage,
		StoragePercent:   m.StoragePercent,
		Timestamp:        m.Timestamp.UnixMilli(),
		NetworkIn:        m.NetworkIn,
		NetworkOut:       m.NetworkOut,
		LoadAvg_1Min:     m.LoadAvg1min,
		LoadAvg_5Min:     m.LoadAvg5min,
		LoadAvg_15Min:    m.LoadAvg15min,
		SwapUsed:         m.SwapUsed,
		SwapFree:         m.SwapFree,
		SwapPercent:      m.SwapPercent,
		RunningProcesses: m.RunningProcesses,
	}
}

// toProtoAppMetrics converts monitor.AppMetrics to its protobuf
// representation. Mirrors the old "metrics" WebSocket message.
func toProtoAppMetrics(m monitor.AppMetrics) *pb.AppResourceMetrics {
	return &pb.AppResourceMetrics{
		AppId:       m.AppID,
		ServerId:    m.ServerID,
		CpuUsage:    m.CPUUsage,
		MemoryUsage: m.MemoryUsage,
	}
}

// toProtoAppInfo converts monitor.AppDetails to its protobuf representation.
// Mirrors the old "apps" WebSocket message.
func toProtoAppInfo(a monitor.AppDetails) *pb.ApplicationInfo {
	return &pb.ApplicationInfo{
		Id:          a.ID,
		ServerId:    a.ServerID,
		Name:        a.Name,
		Status:      a.Status,
		Language:    string(a.Language),
		Port:        int32(a.Port),
		BuildStatus: a.BuildStatus,
		Pid:         int32(a.PID),
		Uptime:      a.Uptime,
		CreatedAt:   a.CreatedAt.UnixMilli(),
		UpdatedAt:   a.UpdatedAt.UnixMilli(),
	}
}

// toProtoLogEntry converts logs.LogEntry to its protobuf representation.
// Mirrors the old "app_logs"/"self_logs" WebSocket messages, distinguished
// by the LogSource field.
func toProtoLogEntry(e logs.LogEntry, source pb.LogSource) *pb.MonitorLogEntry {
	return &pb.MonitorLogEntry{
		Id:       e.ID,
		ServerId: e.ServerID,
		AppId:    e.AppID,
		Log:      e.Log,
		Date:     e.Date.UnixMilli(),
		Level:    string(e.Level),
		Source:   source,
	}
}
