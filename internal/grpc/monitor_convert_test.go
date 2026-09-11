package grpc

import (
	"strings"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/server"
)

func TestToProtoServerInfo(t *testing.T) {
	reboot := time.Now().Add(-24 * time.Hour).Truncate(time.Millisecond)
	created := time.Now().Truncate(time.Millisecond)

	info := &server.Info{
		ID:            "srv-1",
		Hostname:      "host-1",
		IPv4:          "10.0.0.1",
		IPv6:          "::1",
		OSType:        "linux",
		OSFull:        "linux,ubuntu 24.04",
		CPUInfo:       "Some CPU",
		NetworkIfaces: []string{"eth0(aa:bb)"},
		TotalMemory:   1024,
		TotalCPUCores: 8,
		TotalStorage:  2048,
		Uptime:        "1d 2h 3m",
		LastReboot:    reboot,
		Status:        "healthy",
		CreatedAt:     created,
		Region:        "us-east",
		Architecture:  "amd64",
		KernelVersion: "6.1.0",
		SwapTotal:     512,
	}

	got := toProtoServerInfo(info)
	if got == nil {
		t.Fatal("expected non-nil ServerInfo")
	}

	if got.GetId() != info.ID || got.GetHostname() != info.Hostname ||
		got.GetIpV4() != info.IPv4 || got.GetIpV6() != info.IPv6 ||
		got.GetOsType() != info.OSType || got.GetOsFull() != info.OSFull ||
		got.GetCpuInfo() != info.CPUInfo || got.GetTotalMemory() != info.TotalMemory ||
		got.GetTotalCpuCores() != int32(info.TotalCPUCores) ||
		got.GetTotalStorage() != info.TotalStorage || got.GetUptime() != info.Uptime ||
		got.GetStatus() != info.Status || got.GetRegion() != info.Region ||
		got.GetArchitecture() != info.Architecture ||
		got.GetKernelVersion() != info.KernelVersion || got.GetSwapTotal() != info.SwapTotal {
		t.Fatalf("field mismatch: %+v vs %+v", got, info)
	}

	if got.GetLastReboot() != reboot.UnixMilli() {
		t.Fatalf("expected LastReboot %d, got %d", reboot.UnixMilli(), got.GetLastReboot())
	}
	if got.GetCreatedAt() != created.UnixMilli() {
		t.Fatalf("expected CreatedAt %d, got %d", created.UnixMilli(), got.GetCreatedAt())
	}
	if len(got.GetNetworkIfaces()) != 1 || got.GetNetworkIfaces()[0] != "eth0(aa:bb)" {
		t.Fatalf("unexpected NetworkIfaces: %v", got.GetNetworkIfaces())
	}
}

func TestToProtoServerInfo_Nil(t *testing.T) {
	if got := toProtoServerInfo(nil); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestToProtoServerInfo_NilSettingsGroups(t *testing.T) {
	// A server.Info without settings must convert to a ServerInfo whose
	// connection/alert/security groups are nil (absent on the wire).
	info := &server.Info{
		ID:       "srv-1",
		Hostname: "host-1",
	}
	got := toProtoServerInfo(info)
	if got.GetConnection() != nil || got.GetAlert() != nil || got.GetSecurity() != nil {
		t.Fatalf("expected nil settings groups, got connection=%v alert=%v security=%v",
			got.GetConnection(), got.GetAlert(), got.GetSecurity())
	}
}

func TestToProtoServerInfo_WithSettings(t *testing.T) {
	// Synthetic passthrough fixtures assembled at runtime — the converter only
	// copies these fields verbatim, so no credential material belongs here.
	passwordFixture := strings.Repeat("pw", 8)
	keyFixture := "fixture-" + strings.Repeat("k", 16)

	info := &server.Info{
		ID:       "srv-1",
		Hostname: "host-1",
		Connection: &server.ServerConnection{
			SSHPort:     2222,
			SSHUser:     "deploy",
			AuthMethod:  "key",
			SSHPassword: passwordFixture,
			PrivateKey:  keyFixture,
			PublicKey:   "ssh-ed25519 AAAA... deploy",
		},
		Alert: &server.ServerAlert{
			CPUThreshold:          80,
			RAMThreshold:          90,
			DiskThreshold:         85,
			CPUSpikeAlerts:        true,
			MemoryPressureAlerts:  false,
			DiskSpaceAlerts:       true,
			AppCrashAlerts:        true,
			AgentDisconnectAlerts: false,
			WeeklyDigest:          true,
		},
		Security: &server.ServerSecurity{
			FirewallEnabled:    true,
			AutoUpdates:        true,
			SSHRootLogin:       "prohibit-password",
			IPAllowlistEnabled: true,
			AllowedIPs:         []string{"10.0.0.0/8", "192.168.1.0/24"},
			OpenPorts:          []int32{22, 80, 443},
		},
	}

	got := toProtoServerInfo(info)
	if got.GetConnection() == nil || got.GetAlert() == nil || got.GetSecurity() == nil {
		t.Fatal("expected all three settings groups to be populated")
	}

	// Connection. ssh_password/private_key are write-only dashboard fields
	// (backend audit H3, 2026-09-11): the backend drops and security-logs
	// them on ingest, so the converter must blank them even when the input
	// carries values. The non-secret fields keep flowing.
	c := got.GetConnection()
	if c.GetSshPort() != 2222 || c.GetSshUser() != "deploy" || c.GetAuthMethod() != "key" ||
		c.GetPublicKey() != "ssh-ed25519 AAAA... deploy" {
		t.Fatalf("connection mismatch: %+v", c)
	}
	if c.GetSshPassword() != "" {
		t.Fatalf("ssh_password must never reach the wire, got %q", c.GetSshPassword())
	}
	if c.GetPrivateKey() != "" {
		t.Fatalf("private_key must never reach the wire, got %q", c.GetPrivateKey())
	}

	// Alert.
	a := got.GetAlert()
	if a.GetCpuThreshold() != 80 || a.GetRamThreshold() != 90 || a.GetDiskThreshold() != 85 ||
		!a.GetCpuSpikeAlerts() || a.GetMemoryPressureAlerts() || !a.GetDiskSpaceAlerts() ||
		!a.GetAppCrashAlerts() || a.GetAgentDisconnectAlerts() || !a.GetWeeklyDigest() {
		t.Fatalf("alert mismatch: %+v", a)
	}

	// Security.
	s := got.GetSecurity()
	if !s.GetFirewallEnabled() || !s.GetAutoUpdates() || s.GetSshRootLogin() != "prohibit-password" ||
		!s.GetIpAllowlistEnabled() {
		t.Fatalf("security mismatch: %+v", s)
	}
	if len(s.GetAllowedIps()) != 2 || s.GetAllowedIps()[0] != "10.0.0.0/8" || s.GetAllowedIps()[1] != "192.168.1.0/24" {
		t.Fatalf("allowed_ips mismatch: %v", s.GetAllowedIps())
	}
	if len(s.GetOpenPorts()) != 3 || s.GetOpenPorts()[0] != 22 || s.GetOpenPorts()[1] != 80 || s.GetOpenPorts()[2] != 443 {
		t.Fatalf("open_ports mismatch: %v", s.GetOpenPorts())
	}
}

func TestToProtoServerMetrics(t *testing.T) {
	ts := time.Now().Truncate(time.Millisecond)
	m := &server.Metrics{
		ServerID:         "srv-1",
		UsedMemory:       100,
		FreeMemory:       200,
		MemoryPercent:    33.3,
		CPUUsagePercent:  12.5,
		UsedStorage:      300,
		FreeStorage:      400,
		StoragePercent:   42.0,
		Timestamp:        ts,
		NetworkIn:        500,
		NetworkOut:       600,
		LoadAvg1min:      0.1,
		LoadAvg5min:      0.2,
		LoadAvg15min:     0.3,
		SwapUsed:         10,
		SwapFree:         20,
		SwapPercent:      50.0,
		RunningProcesses: 42,
	}

	got := toProtoServerMetrics(m)
	if got == nil {
		t.Fatal("expected non-nil ServerMetrics")
	}
	if got.GetServerId() != m.ServerID || got.GetUsedMemory() != m.UsedMemory ||
		got.GetFreeMemory() != m.FreeMemory || got.GetMemoryPercent() != m.MemoryPercent ||
		got.GetCpuUsagePercent() != m.CPUUsagePercent || got.GetUsedStorage() != m.UsedStorage ||
		got.GetFreeStorage() != m.FreeStorage || got.GetStoragePercent() != m.StoragePercent ||
		got.GetNetworkIn() != m.NetworkIn || got.GetNetworkOut() != m.NetworkOut ||
		got.GetLoadAvg_1Min() != m.LoadAvg1min || got.GetLoadAvg_5Min() != m.LoadAvg5min ||
		got.GetLoadAvg_15Min() != m.LoadAvg15min || got.GetSwapUsed() != m.SwapUsed ||
		got.GetSwapFree() != m.SwapFree || got.GetSwapPercent() != m.SwapPercent ||
		got.GetRunningProcesses() != m.RunningProcesses {
		t.Fatalf("field mismatch: %+v vs %+v", got, m)
	}
	if got.GetTimestamp() != ts.UnixMilli() {
		t.Fatalf("expected Timestamp %d, got %d", ts.UnixMilli(), got.GetTimestamp())
	}
}

func TestToProtoAppMetrics(t *testing.T) {
	m := monitor.AppMetrics{
		AppID:       "app-1",
		ServerID:    "srv-1",
		CPUUsage:    5.5,
		MemoryUsage: 1024,
	}
	got := toProtoAppMetrics(m)
	if got.GetAppId() != m.AppID || got.GetServerId() != m.ServerID ||
		got.GetCpuUsage() != m.CPUUsage || got.GetMemoryUsage() != m.MemoryUsage {
		t.Fatalf("field mismatch: %+v vs %+v", got, m)
	}
}

func TestToProtoAppInfo(t *testing.T) {
	created := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	updated := time.Now().Truncate(time.Millisecond)
	a := monitor.AppDetails{
		ID:          "app-1",
		ServerID:    "srv-1",
		Name:        "myapp",
		Status:      "running",
		Language:    "go",
		Port:        8080,
		BuildStatus: "success",
		PID:         1234,
		Uptime:      "2h",
		CreatedAt:   created,
		UpdatedAt:   updated,
	}
	got := toProtoAppInfo(a)
	if got.GetId() != a.ID || got.GetServerId() != a.ServerID || got.GetName() != a.Name ||
		got.GetStatus() != a.Status || got.GetLanguage() != string(a.Language) ||
		got.GetPort() != int32(a.Port) || got.GetBuildStatus() != a.BuildStatus ||
		got.GetPid() != int32(a.PID) || got.GetUptime() != a.Uptime {
		t.Fatalf("field mismatch: %+v vs %+v", got, a)
	}
	if got.GetCreatedAt() != created.UnixMilli() || got.GetUpdatedAt() != updated.UnixMilli() {
		t.Fatalf("timestamp mismatch: %+v vs %+v", got, a)
	}
}

func TestToProtoAppInfo_WithConfig(t *testing.T) {
	created := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	updated := time.Now().Truncate(time.Millisecond)

	a := monitor.AppDetails{
		ID:          "app-1",
		ServerID:    "srv-1",
		Name:        "myapp",
		Status:      "running",
		Language:    "go",
		Port:        8080,
		BuildStatus: "success",
		PID:         1234,
		Uptime:      "2h",
		CreatedAt:   created,
		UpdatedAt:   updated,
		Process: app.AppProcessConfig{
			WorkingDir:         "/srv/myapp",
			Executable:         "/srv/myapp/app_1",
			AutoRestart:        true,
			MaxCPUPercent:      80,
			MaxMemoryMB:        512,
			MaxOpenFiles:       2048,
			MaxProcesses:       5,
			CrashLoopBackoff:   10,
			GracefulShutdown:   30,
			MaxRestartAttempts: 3,
			RestartDelayMs:     500,
		},
		Networking: app.AppNetworkingConfig{
			ListenPort:       8080,
			BindAddress:      "0.0.0.0",
			PublicDomain:     "myapp.example.com",
			BasePath:         "/v1",
			TLSEnabled:       true,
			CertPath:         "/certs/tls.crt",
			KeyPath:          "/certs/tls.key",
			ProxyEnabled:     true,
			CORSEnabled:      true,
			AllowedOrigins:   []string{"https://app.example.com"},
			RateLimitEnabled: true,
			RateLimitRPS:     100,
		},
		Logging: app.AppLoggingConfig{
			JSONLogs:          true,
			PersistentLogs:    true,
			LogLevel:          "info",
			LogFilePath:       "/srv/myapp/logs/app.log",
			StderrFilePath:    "/srv/myapp/logs/app.err.log",
			LogFormat:         "json",
			RotationEnabled:   true,
			RotationMaxSizeMB: 100,
			RotationMaxFiles:  5,
			RotationCompress:  true,
		},
		Storage: app.AppStorageConfig{
			Volumes: []string{"/data:/data", "/cache:/cache"},
			DataDir: "/srv/myapp/data",
			TempDir: "/tmp/myapp",
		},
	}

	got := toProtoAppInfo(a)
	if got.GetProcess() == nil || got.GetNetworking() == nil || got.GetLogging() == nil || got.GetStorage() == nil {
		t.Fatal("expected all four config groups to be populated")
	}

	// Process.
	p := got.GetProcess()
	if p.GetWorkingDir() != "/srv/myapp" || p.GetExecutable() != "/srv/myapp/app_1" ||
		!p.GetAutoRestart() || p.GetMaxCpuPercent() != 80 || p.GetMaxMemoryMb() != 512 ||
		p.GetMaxOpenFiles() != 2048 || p.GetMaxProcesses() != 5 || p.GetCrashLoopBackoff() != 10 ||
		p.GetGracefulShutdown() != 30 || p.GetMaxRestartAttempts() != 3 || p.GetRestartDelayMs() != 500 {
		t.Fatalf("process mismatch: %+v", p)
	}

	// Networking.
	n := got.GetNetworking()
	if n.GetListenPort() != 8080 || n.GetBindAddress() != "0.0.0.0" ||
		n.GetPublicDomain() != "myapp.example.com" || n.GetBasePath() != "/v1" ||
		!n.GetTlsEnabled() || n.GetCertPath() != "/certs/tls.crt" || n.GetKeyPath() != "/certs/tls.key" ||
		!n.GetProxyEnabled() || !n.GetCorsEnabled() || !n.GetRateLimitEnabled() ||
		n.GetRateLimitRps() != 100 {
		t.Fatalf("networking mismatch: %+v", n)
	}
	if len(n.GetAllowedOrigins()) != 1 || n.GetAllowedOrigins()[0] != "https://app.example.com" {
		t.Fatalf("allowed_origins mismatch: %v", n.GetAllowedOrigins())
	}

	// Logging.
	l := got.GetLogging()
	if !l.GetJsonLogs() || !l.GetPersistentLogs() || l.GetLogLevel() != "info" ||
		l.GetLogFilePath() != "/srv/myapp/logs/app.log" || l.GetStderrFilePath() != "/srv/myapp/logs/app.err.log" ||
		l.GetLogFormat() != "json" || !l.GetRotationEnabled() || l.GetRotationMaxSizeMb() != 100 ||
		l.GetRotationMaxFiles() != 5 || !l.GetRotationCompress() {
		t.Fatalf("logging mismatch: %+v", l)
	}

	// Storage.
	s := got.GetStorage()
	if s.GetDataDir() != "/srv/myapp/data" || s.GetTempDir() != "/tmp/myapp" {
		t.Fatalf("storage mismatch: %+v", s)
	}
	if len(s.GetVolumes()) != 2 || s.GetVolumes()[0] != "/data:/data" || s.GetVolumes()[1] != "/cache:/cache" {
		t.Fatalf("volumes mismatch: %v", s.GetVolumes())
	}
}

func TestToProtoAppInfo_EmptyConfigGroupsAreNil(t *testing.T) {
	a := monitor.AppDetails{ID: "app-1", ServerID: "srv-1"}
	got := toProtoAppInfo(a)
	// A zero-value config group converts to a non-nil message with all fields
	// zeroed (the CLI has no explicit configuration to report).
	if got.GetProcess() == nil {
		t.Fatal("expected a non-nil AppProcess (zero fields) for an unset config")
	}
	if got.GetProcess().GetAutoRestart() {
		t.Fatal("expected default AutoRestart = false")
	}
}

func TestToProtoLogEntry(t *testing.T) {
	date := time.Now().Truncate(time.Millisecond)
	e := logs.LogEntry{
		ID:       "log-1",
		ServerID: "srv-1",
		AppID:    "app-1",
		Log:      "something happened",
		Date:     date,
		Level:    logs.LevelError,
		Stream:   logs.StreamStdout,
	}

	got := toProtoLogEntry(e, pb.LogSource_LOG_SOURCE_APPLICATION)
	if got.GetId() != e.ID || got.GetServerId() != e.ServerID || got.GetAppId() != e.AppID ||
		got.GetLog() != e.Log || got.GetLevel() != string(e.Level) {
		t.Fatalf("field mismatch: %+v vs %+v", got, e)
	}
	if got.GetDate() != date.UnixMilli() {
		t.Fatalf("expected Date %d, got %d", date.UnixMilli(), got.GetDate())
	}
	if got.GetSource() != pb.LogSource_LOG_SOURCE_APPLICATION {
		t.Fatalf("expected source APPLICATION, got %v", got.GetSource())
	}
	// App logs carry the stream that produced the line; component stays empty.
	if got.GetStream() != pb.LogStream_LOG_STREAM_STDOUT {
		t.Fatalf("expected stream STDOUT, got %v", got.GetStream())
	}
	if got.GetComponent() != "" {
		t.Fatalf("expected empty component for app log, got %q", got.GetComponent())
	}

	// A stderr app line maps to LOG_STREAM_STDERR.
	stderrEntry := logs.LogEntry{
		ID:     "log-2",
		Log:    "boom",
		Date:   time.Now(),
		Level:  logs.LevelError,
		Stream: logs.StreamStderr,
	}
	stderrGot := toProtoLogEntry(stderrEntry, pb.LogSource_LOG_SOURCE_APPLICATION)
	if stderrGot.GetStream() != pb.LogStream_LOG_STREAM_STDERR {
		t.Fatalf("expected stream STDERR, got %v", stderrGot.GetStream())
	}
}

func TestToProtoLogEntry_Self(t *testing.T) {
	date := time.Now().Truncate(time.Millisecond)
	e := logs.LogEntry{
		ID:        "log-3",
		ServerID:  "srv-1",
		Log:       "monitor tick failed",
		Date:      date,
		Level:     logs.LevelWarning,
		Component: "monitor",
	}

	selfEntry := toProtoLogEntry(e, pb.LogSource_LOG_SOURCE_SELF)
	if selfEntry.GetSource() != pb.LogSource_LOG_SOURCE_SELF {
		t.Fatalf("expected source SELF, got %v", selfEntry.GetSource())
	}
	// Self logs carry the subsystem component; stream stays UNSPECIFIED.
	if selfEntry.GetComponent() != "monitor" {
		t.Fatalf("expected component %q, got %q", "monitor", selfEntry.GetComponent())
	}
	if selfEntry.GetStream() != pb.LogStream_LOG_STREAM_UNSPECIFIED {
		t.Fatalf("expected stream UNSPECIFIED for self log, got %v", selfEntry.GetStream())
	}
}

func TestToProtoLogStream(t *testing.T) {
	cases := []struct {
		in   logs.LogStream
		want pb.LogStream
	}{
		{logs.StreamUnknown, pb.LogStream_LOG_STREAM_UNSPECIFIED},
		{logs.StreamStdout, pb.LogStream_LOG_STREAM_STDOUT},
		{logs.StreamStderr, pb.LogStream_LOG_STREAM_STDERR},
		{logs.LogStream("bogus"), pb.LogStream_LOG_STREAM_UNSPECIFIED},
	}
	for _, c := range cases {
		if got := toProtoLogStream(c.in); got != c.want {
			t.Errorf("toProtoLogStream(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
