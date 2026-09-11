package grpc

import (
	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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
	out := &pb.ServerInfo{
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

	// Server-settings groups (the server's real configuration). Nil groups
	// stay nil on the wire — same semantics as ApplicationInfo's config
	// groups.
	out.Connection = toProtoServerConnection(info.Connection)
	out.Alert = toProtoServerAlert(info.Alert)
	out.Security = toProtoServerSecurity(info.Security)

	return out
}

// toProtoServerConnection converts the Connection settings to its protobuf
// representation. The group is always empty — the agent does not collect SSH
// connection details or key material (see server.ServerConnection) — but it is
// still sent so the message shape stays stable.
//
// H3 (2026-09-11 hardening): ssh_password and private_key are write-only
// dashboard fields. The backend drops them on ingest and security-logs their
// presence, so they must never be serialized here. The struct fields stay
// (the persisted settings model still carries the group); the wire copy is
// forcibly blanked as a defense-in-depth chokepoint even if some future code
// path starts populating them.
func toProtoServerConnection(c *server.ServerConnection) *pb.ServerConnection {
	if c == nil {
		return nil
	}
	return &pb.ServerConnection{
		SshPort:     int32(c.SSHPort),
		SshUser:     c.SSHUser,
		AuthMethod:  c.AuthMethod,
		SshPassword: "", // write-only on the backend; never sent (H3)
		PrivateKey:  "", // write-only on the backend; never sent (H3)
		PublicKey:   c.PublicKey,
	}
}

// toProtoServerAlert converts the Alert settings to its protobuf
// representation.
func toProtoServerAlert(a *server.ServerAlert) *pb.ServerAlert {
	if a == nil {
		return nil
	}
	return &pb.ServerAlert{
		CpuThreshold:          a.CPUThreshold,
		RamThreshold:          a.RAMThreshold,
		DiskThreshold:         a.DiskThreshold,
		CpuSpikeAlerts:        a.CPUSpikeAlerts,
		MemoryPressureAlerts:  a.MemoryPressureAlerts,
		DiskSpaceAlerts:       a.DiskSpaceAlerts,
		AppCrashAlerts:        a.AppCrashAlerts,
		AgentDisconnectAlerts: a.AgentDisconnectAlerts,
		WeeklyDigest:          a.WeeklyDigest,
	}
}

// toProtoServerSecurity converts the Security settings to its protobuf
// representation.
func toProtoServerSecurity(s *server.ServerSecurity) *pb.ServerSecurity {
	if s == nil {
		return nil
	}
	return &pb.ServerSecurity{
		FirewallEnabled:    s.FirewallEnabled,
		AutoUpdates:        s.AutoUpdates,
		SshRootLogin:       s.SSHRootLogin,
		IpAllowlistEnabled: s.IPAllowlistEnabled,
		AllowedIps:         s.AllowedIPs,
		OpenPorts:          s.OpenPorts,
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
// Mirrors the old "apps" WebSocket message. The configuration groups
// (process/networking/logging/storage) are converted to their proto
// counterparts and attached; they are omitted when the app has no
// configuration to report.
func toProtoAppInfo(a monitor.AppDetails) *pb.ApplicationInfo {
	info := &pb.ApplicationInfo{
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

	info.Process = toProtoAppProcess(a.Process)
	info.Networking = toProtoAppNetworking(a.Networking)
	info.Logging = toProtoAppLogging(a.Logging)
	info.Storage = toProtoAppStorage(a.Storage)

	return info
}

// toProtoAppProcess converts app.AppProcessConfig to its protobuf
// representation.
func toProtoAppProcess(p app.AppProcessConfig) *pb.AppProcess {
	return &pb.AppProcess{
		WorkingDir:         p.WorkingDir,
		Executable:         p.Executable,
		StartCommand:       p.StartCommand,
		StopCommand:        p.StopCommand,
		MaxCpuPercent:      int32(p.MaxCPUPercent),
		MaxMemoryMb:        p.MaxMemoryMB,
		MaxOpenFiles:       p.MaxOpenFiles,
		MaxProcesses:       p.MaxProcesses,
		AutoRestart:        p.AutoRestart,
		CrashLoopBackoff:   int32(p.CrashLoopBackoff),
		GracefulShutdown:   int32(p.GracefulShutdown),
		MaxRestartAttempts: int32(p.MaxRestartAttempts),
		RestartDelayMs:     int32(p.RestartDelayMs),
	}
}

// toProtoAppNetworking converts app.AppNetworkingConfig to its protobuf
// representation.
func toProtoAppNetworking(n app.AppNetworkingConfig) *pb.AppNetworking {
	return &pb.AppNetworking{
		ListenPort:       int32(n.ListenPort),
		BindAddress:      n.BindAddress,
		PublicDomain:     n.PublicDomain,
		BasePath:         n.BasePath,
		TlsEnabled:       n.TLSEnabled,
		CertPath:         n.CertPath,
		KeyPath:          n.KeyPath,
		ProxyEnabled:     n.ProxyEnabled,
		CorsEnabled:      n.CORSEnabled,
		AllowedOrigins:   n.AllowedOrigins,
		RateLimitEnabled: n.RateLimitEnabled,
		RateLimitRps:     int32(n.RateLimitRPS),
	}
}

// toProtoAppLogging converts app.AppLoggingConfig to its protobuf
// representation.
func toProtoAppLogging(l app.AppLoggingConfig) *pb.AppLogging {
	return &pb.AppLogging{
		JsonLogs:          l.JSONLogs,
		PersistentLogs:    l.PersistentLogs,
		LogLevel:          l.LogLevel,
		LogFilePath:       l.LogFilePath,
		StderrFilePath:    l.StderrFilePath,
		LogFormat:         l.LogFormat,
		RotationEnabled:   l.RotationEnabled,
		RotationMaxSizeMb: int32(l.RotationMaxSizeMB),
		RotationMaxFiles:  int32(l.RotationMaxFiles),
		RotationCompress:  l.RotationCompress,
	}
}

// toProtoAppStorage converts app.AppStorageConfig to its protobuf
// representation.
func toProtoAppStorage(s app.AppStorageConfig) *pb.AppStorage {
	return &pb.AppStorage{
		Volumes: s.Volumes,
		DataDir: s.DataDir,
		TempDir: s.TempDir,
	}
}

// toProtoLogStream maps logs.LogStream (string) to the proto LogStream enum.
// The empty string (StreamUnknown, used by self logs) maps to the proto's
// UNSPECIFIED zero value so self-log entries are unambiguous on the wire.
func toProtoLogStream(s logs.LogStream) pb.LogStream {
	switch s {
	case logs.StreamStdout:
		return pb.LogStream_LOG_STREAM_STDOUT
	case logs.StreamStderr:
		return pb.LogStream_LOG_STREAM_STDERR
	default:
		return pb.LogStream_LOG_STREAM_UNSPECIFIED
	}
}

// toProtoLogEntry converts logs.LogEntry to its protobuf representation.
// Mirrors the old "app_logs"/"self_logs" WebSocket messages, distinguished
// by the LogSource field. App logs carry stream (stdout/stderr); self logs
// carry component (which subsystem produced the line). The two are mutually
// exclusive by construction — component is only set on self-log entries and
// stream only on app-log entries — so the backend can key display and rules
// off source alone.
//
// The log body is redacted before it hits the wire: application logs contain
// arbitrary user output (DATABASE_URL values, printed tokens, key material)
// and self logs can embed lower-layer errors. This is the single chokepoint
// every monitor-stream log passes through, so redaction here covers both
// sources on every tick and every replay.
func toProtoLogEntry(e logs.LogEntry, source pb.LogSource) *pb.MonitorLogEntry {
	return &pb.MonitorLogEntry{
		Id:        e.ID,
		ServerId:  e.ServerID,
		AppId:     e.AppID,
		Log:       phelixerr.Redact(e.Log),
		Date:      e.Date.UnixMilli(),
		Level:     string(e.Level),
		Source:    source,
		Stream:    toProtoLogStream(e.Stream),
		Component: e.Component,
	}
}
