package auth

import "time"

// Session holds the user's session information.
type Session struct {
	SessionID string    `json:"sessionID"`
	Token     string    `json:"token"`
	Username  string    `json:"username"`
	UserID    uint      `json:"userID"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// AppProcess mirrors app.AppProcessConfig for the HTTP upload payload.
type AppProcess struct {
	WorkingDir         string `json:"working_dir,omitempty"`
	Executable         string `json:"executable,omitempty"`
	StartCommand       string `json:"start_command,omitempty"`
	StopCommand        string `json:"stop_command,omitempty"`
	MaxCPUPercent      int    `json:"max_cpu_percent,omitempty"`
	MaxMemoryMB        int64  `json:"max_memory_mb,omitempty"`
	MaxOpenFiles       int64  `json:"max_open_files,omitempty"`
	MaxProcesses       int64  `json:"max_processes,omitempty"`
	AutoRestart        bool   `json:"auto_restart,omitempty"`
	CrashLoopBackoff   int    `json:"crash_loop_backoff,omitempty"`
	GracefulShutdown   int    `json:"graceful_shutdown,omitempty"`
	MaxRestartAttempts int    `json:"max_restart_attempts,omitempty"`
	RestartDelayMs     int    `json:"restart_delay_ms,omitempty"`
}

// AppNetworking mirrors app.AppNetworkingConfig for the HTTP upload payload.
type AppNetworking struct {
	ListenPort       int      `json:"listen_port,omitempty"`
	BindAddress      string   `json:"bind_address,omitempty"`
	PublicDomain     string   `json:"public_domain,omitempty"`
	BasePath         string   `json:"base_path,omitempty"`
	TLSEnabled       bool     `json:"tls_enabled,omitempty"`
	CertPath         string   `json:"cert_path,omitempty"`
	KeyPath          string   `json:"key_path,omitempty"`
	ProxyEnabled     bool     `json:"proxy_enabled,omitempty"`
	CORSEnabled      bool     `json:"cors_enabled,omitempty"`
	AllowedOrigins   []string `json:"allowed_origins,omitempty"`
	RateLimitEnabled bool     `json:"rate_limit_enabled,omitempty"`
	RateLimitRPS     int      `json:"rate_limit_rps,omitempty"`
}

// AppLogging mirrors app.AppLoggingConfig for the HTTP upload payload.
type AppLogging struct {
	JSONLogs          bool   `json:"json_logs,omitempty"`
	PersistentLogs    bool   `json:"persistent_logs,omitempty"`
	LogLevel          string `json:"log_level,omitempty"`
	LogFilePath       string `json:"log_file_path,omitempty"`
	StderrFilePath    string `json:"stderr_file_path,omitempty"`
	LogFormat         string `json:"log_format,omitempty"`
	RotationEnabled   bool   `json:"rotation_enabled,omitempty"`
	RotationMaxSizeMB int    `json:"rotation_max_size_mb,omitempty"`
	RotationMaxFiles  int    `json:"rotation_max_files,omitempty"`
	RotationCompress  bool   `json:"rotation_compress,omitempty"`
}

// AppStorage mirrors app.AppStorageConfig for the HTTP upload payload.
type AppStorage struct {
	Volumes []string `json:"volumes,omitempty"`
	DataDir string   `json:"data_dir,omitempty"`
	TempDir string   `json:"temp_dir,omitempty"`
}

// SessionStatus represents the current authentication status, as returned by
// GET /auth/phelix/status.
type SessionStatus struct {
	User       string `json:"user"`
	SessionID  string `json:"sessionID"`
	Device     string `json:"device"`
	CliVersion string `json:"cliVersion"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	ExpiresAt  string `json:"expiresAt"`
}

// AppDetail represents an application's runtime details plus its reported
// configuration groups (process/networking/logging/storage).
type AppDetail struct {
	ID          uint      `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	PID         int       `json:"pid"`
	Uptime      string    `json:"uptime"`
	BuildStatus string    `json:"buildStatus"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`

	Process    AppProcess    `json:"process,omitempty"`
	Networking AppNetworking `json:"networking,omitempty"`
	Logging    AppLogging    `json:"logging,omitempty"`
	Storage    AppStorage    `json:"storage,omitempty"`
}
