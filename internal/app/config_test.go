package app

import (
	"os"
	"testing"
)

func TestAppInfo_Config_SeedsDefaultsFromState(t *testing.T) {
	a := &AppInfo{
		ID:        "42",
		Name:      "myapp",
		Directory: "/srv/myapp",
		Port:      8080,
		LogFile:   "/home/user/.phelix/logs/42.log",
	}

	cfg := a.Config()

	// Process defaults derive from the app's directory/binary layout.
	if cfg.Process.WorkingDir != "/srv/myapp" {
		t.Fatalf("WorkingDir = %q, want %q", cfg.Process.WorkingDir, "/srv/myapp")
	}
	if cfg.Process.Executable != "/srv/myapp/app_42" {
		t.Fatalf("Executable = %q, want %q", cfg.Process.Executable, "/srv/myapp/app_42")
	}

	// Networking defaults to the app port.
	if cfg.Networking.ListenPort != 8080 {
		t.Fatalf("ListenPort = %d, want 8080", cfg.Networking.ListenPort)
	}

	// Logging defaults to persistent capture of the app's log file.
	if cfg.Logging.LogFilePath != "/home/user/.phelix/logs/42.log" {
		t.Fatalf("LogFilePath = %q, want the app log file", cfg.Logging.LogFilePath)
	}
	if !cfg.Logging.PersistentLogs {
		t.Fatal("PersistentLogs should default to true when a log file exists")
	}
	if cfg.Logging.LogLevel != "info" {
		t.Fatalf("LogLevel = %q, want %q", cfg.Logging.LogLevel, "info")
	}

	// Storage defaults to the app directory and system temp.
	if cfg.Storage.DataDir != "/srv/myapp" {
		t.Fatalf("DataDir = %q, want %q", cfg.Storage.DataDir, "/srv/myapp")
	}
	if cfg.Storage.TempDir != os.TempDir() {
		t.Fatalf("TempDir = %q, want %q", cfg.Storage.TempDir, os.TempDir())
	}
}

func TestAppInfo_Config_ExplicitValuesWin(t *testing.T) {
	a := &AppInfo{
		ID:        "7",
		Name:      "custom",
		Directory: "/srv/myapp",
		Port:      8080,
		Process: AppProcessConfig{
			WorkingDir:     "/opt/override",
			Executable:     "/opt/bin/server",
			AutoRestart:    true,
			RestartDelayMs: 2500,
		},
		Networking: AppNetworkingConfig{
			PublicDomain: "myapp.example.com",
			ListenPort:   9000,
		},
		Logging: AppLoggingConfig{
			LogLevel: "debug",
		},
		Storage: AppStorageConfig{
			Volumes: []string{"/data:/data"},
		},
	}

	cfg := a.Config()

	if cfg.Process.WorkingDir != "/opt/override" {
		t.Fatalf("WorkingDir = %q, want the explicit override", cfg.Process.WorkingDir)
	}
	if cfg.Process.Executable != "/opt/bin/server" {
		t.Fatalf("Executable = %q, want the explicit override", cfg.Process.Executable)
	}
	if !cfg.Process.AutoRestart {
		t.Fatal("AutoRestart should be carried through")
	}
	if cfg.Process.RestartDelayMs != 2500 {
		t.Fatalf("RestartDelayMs = %d, want 2500", cfg.Process.RestartDelayMs)
	}
	if cfg.Networking.ListenPort != 9000 {
		t.Fatalf("ListenPort = %d, want the explicit 9000", cfg.Networking.ListenPort)
	}
	if cfg.Networking.PublicDomain != "myapp.example.com" {
		t.Fatalf("PublicDomain = %q, want the explicit value", cfg.Networking.PublicDomain)
	}
	if cfg.Logging.LogLevel != "debug" {
		t.Fatalf("LogLevel = %q, want %q", cfg.Logging.LogLevel, "debug")
	}
	if len(cfg.Storage.Volumes) != 1 || cfg.Storage.Volumes[0] != "/data:/data" {
		t.Fatalf("Volumes = %v, want the explicit mount", cfg.Storage.Volumes)
	}
}
