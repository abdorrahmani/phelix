package proto

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// TestMonitorEvent_RoundTrip verifies that every oneof branch of MonitorEvent
// survives a marshal/unmarshal round trip, which is what actually crosses
// the wire between the CLI monitor daemon and the backend.
func TestMonitorEvent_RoundTrip(t *testing.T) {
	cases := []*MonitorEvent{
		{
			ServerId:  "srv-1",
			Timestamp: 1000,
			Payload: &MonitorEvent_ServerInfo{
				ServerInfo: &ServerInfo{
					Id:            "srv-1",
					Hostname:      "host-1",
					NetworkIfaces: []string{"eth0"},
					TotalCpuCores: 4,
					Connection: &ServerConnection{
						SshPort:     22,
						SshUser:     "phelix",
						AuthMethod:  "key",
						PrivateKey:  "prv",
						PublicKey:   "pub",
						SshPassword: "pw",
					},
					Alert: &ServerAlert{
						CpuThreshold:   80,
						RamThreshold:   90,
						DiskThreshold:  90,
						AppCrashAlerts: true,
						WeeklyDigest:   true,
					},
					Security: &ServerSecurity{
						FirewallEnabled: true,
						AutoUpdates:     true,
						SshRootLogin:    "prohibit-password",
						AllowedIps:      []string{"10.0.0.0/8"},
						OpenPorts:       []int32{22, 80, 443},
					},
				},
			},
		},
		{
			ServerId:  "srv-1",
			Timestamp: 2000,
			Payload: &MonitorEvent_ServerMetrics{
				ServerMetrics: &ServerMetrics{
					ServerId:        "srv-1",
					CpuUsagePercent: 12.5,
					UsedMemory:      1024,
				},
			},
		},
		{
			ServerId:  "srv-1",
			Timestamp: 3000,
			Payload: &MonitorEvent_AppMetrics{
				AppMetrics: &AppResourceMetrics{
					AppId:       "app-1",
					ServerId:    "srv-1",
					CpuUsage:    1.5,
					MemoryUsage: 2048,
				},
			},
		},
		{
			ServerId:  "srv-1",
			Timestamp: 4000,
			Payload: &MonitorEvent_AppInfo{
				AppInfo: &ApplicationInfo{
					Id:       "app-1",
					ServerId: "srv-1",
					Name:     "myapp",
					Status:   "running",
					Pid:      123,
					Process: &AppProcess{
						WorkingDir:    "/srv/myapp",
						Executable:    "/srv/myapp/app_1",
						AutoRestart:   true,
						MaxCpuPercent: 80,
					},
					Networking: &AppNetworking{
						ListenPort:     8080,
						PublicDomain:   "myapp.example.com",
						AllowedOrigins: []string{"https://app.example.com"},
					},
					Logging: &AppLogging{
						LogLevel:          "info",
						PersistentLogs:    true,
						RotationMaxSizeMb: 100,
					},
					Storage: &AppStorage{
						Volumes: []string{"/data:/data"},
						DataDir: "/srv/myapp/data",
						TempDir: "/tmp/myapp",
					},
				},
			},
		},
		{
			ServerId:  "srv-1",
			Timestamp: 5000,
			Payload: &MonitorEvent_LogEntry{
				LogEntry: &MonitorLogEntry{
					Id:       "log-1",
					ServerId: "srv-1",
					Log:      "hello",
					Source:   LogSource_LOG_SOURCE_SELF,
				},
			},
		},
		{
			ServerId:  "srv-1",
			Timestamp: 6000,
			Payload: &MonitorEvent_CommandResult{
				CommandResult: &MonitorCommandResult{
					RequestId: "req-1",
					Command:   "restart",
					AppName:   "myapp",
					Status:    "success",
				},
			},
		},
		{
			ServerId:  "srv-1",
			Timestamp: 7000,
			Payload: &MonitorEvent_Pong{
				Pong: &Pong{Id: "ping-1", Timestamp: 7000},
			},
		},
	}

	for _, want := range cases {
		data, err := proto.Marshal(want)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}

		got := &MonitorEvent{}
		if err := proto.Unmarshal(data, got); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}

		if !proto.Equal(want, got) {
			t.Fatalf("round trip mismatch:\nwant: %v\ngot:  %v", want, got)
		}
	}
}

// TestMonitorControl_RoundTrip verifies the backend-to-CLI control message
// (remote commands + keepalive pings) survives a marshal/unmarshal round
// trip.
func TestMonitorControl_RoundTrip(t *testing.T) {
	cases := []*MonitorControl{
		{
			Payload: &MonitorControl_Command{
				Command: &MonitorCommandRequest{
					RequestId: "req-1",
					Type:      "restart",
					AppName:   "myapp",
				},
			},
		},
		{
			Payload: &MonitorControl_Ping{
				Ping: &Ping{Id: "ping-1", Timestamp: 42},
			},
		},
	}

	for _, want := range cases {
		data, err := proto.Marshal(want)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}

		got := &MonitorControl{}
		if err := proto.Unmarshal(data, got); err != nil {
			t.Fatalf("unmarshal failed: %v", err)
		}

		if !proto.Equal(want, got) {
			t.Fatalf("round trip mismatch:\nwant: %v\ngot:  %v", want, got)
		}
	}
}

// TestLogSource_EnumValues locks in the wire-stable enum values so backend
// and CLI implementations can never silently drift apart.
func TestLogSource_EnumValues(t *testing.T) {
	tests := map[LogSource]int32{
		LogSource_LOG_SOURCE_UNSPECIFIED: 0,
		LogSource_LOG_SOURCE_APPLICATION: 1,
		LogSource_LOG_SOURCE_SELF:        2,
	}
	for source, want := range tests {
		if int32(source) != want {
			t.Fatalf("expected %v to equal %d, got %d", source, want, int32(source))
		}
	}
}
