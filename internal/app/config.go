package app

import (
	"os"
	"path/filepath"
)

// AppConfig contains the four configuration groups reported to the backend
// for a single managed app. It is the payload that crosses both transports
// (gRPC MonitorStream ApplicationInfo.process/networking/logging/storage and
// the HTTP /phelix/apps upload).
type AppConfig struct {
	Process    AppProcessConfig
	Networking AppNetworkingConfig
	Logging    AppLoggingConfig
	Storage    AppStorageConfig
}

// Config returns the effective configuration for the app, seeding any group
// that was not explicitly set with sensible defaults derived from the app's
// own state (directory, port, log file, ...). Explicitly-set values always
// win.
func (a *AppInfo) Config() AppConfig {
	return AppConfig{
		Process:    a.effectiveProcess(),
		Networking: a.effectiveNetworking(),
		Logging:    a.effectiveLogging(),
		Storage:    a.effectiveStorage(),
	}
}

// effectiveProcess returns the process config, defaulting to the app's
// directory and binary layout.
func (a *AppInfo) effectiveProcess() AppProcessConfig {
	p := a.Process

	// Defaults are only applied to unset fields.
	if p.WorkingDir == "" {
		p.WorkingDir = a.Directory
	}
	if p.Executable == "" {
		// Mirror the binary layout in manager.startApplicationProcess.
		p.Executable = filepath.Join(a.Directory, "app_"+a.ID)
	}
	return p
}

// effectiveNetworking returns the networking config, defaulting to the app's
// configured port.
func (a *AppInfo) effectiveNetworking() AppNetworkingConfig {
	n := a.Networking

	if n.ListenPort == 0 {
		n.ListenPort = a.Port
	}
	return n
}

// effectiveLogging returns the logging config, defaulting to the app's log
// file and persistent capture.
func (a *AppInfo) effectiveLogging() AppLoggingConfig {
	l := a.Logging

	if l.LogFilePath == "" {
		l.LogFilePath = a.LogFile
	}
	if !l.PersistentLogs && a.LogFile != "" {
		// The CLI already captures output to a per-app file, so persistent
		// capture is effectively on unless explicitly disabled.
		l.PersistentLogs = true
	}
	if l.LogLevel == "" {
		l.LogLevel = "info"
	}
	return l
}

// effectiveStorage returns the storage config, defaulting data dir to the
// app's working directory.
func (a *AppInfo) effectiveStorage() AppStorageConfig {
	s := a.Storage

	if s.DataDir == "" {
		s.DataDir = a.Directory
	}
	if s.TempDir == "" {
		if dir := os.TempDir(); dir != "" {
			s.TempDir = dir
		}
	}
	return s
}
