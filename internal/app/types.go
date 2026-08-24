package app

import (
	"os"
	"os/exec"
	"time"
)

// AppManagerInterface defines the contract for application management
type AppManagerInterface interface {
	GenerateAppID() string
	StartApplication(id string, port int, name string) error
	StopApplication(id string) error
	RestartApplication(id string) error
	StatusApplication(id string) (AppStatus, error)
	ListApplications() []AppListItem
	SaveState() error
	LoadState() error
	RemoveApplication(id string) error
	RestoreAutoStartApps() (int, error)
}

// AppInfo represents the state of a single application
type AppInfo struct {
	ID          string
	Name        string
	Cmd         *exec.Cmd
	PID         int
	Status      string
	Start       time.Time
	Port        int
	LogFile     string
	BuildStatus string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Directory   string // Directory where the application is located
	// logFileHandle is the open handle backing the app's stdout/stderr. It is
	// held for the lifetime of the running child (see startApplicationProcess)
	// and never persisted.
	logFileHandle *os.File
	// Language represents the programming language of the project (go, rust, ...)
	Language string
	// NoUpload indicates whether this app should be uploaded/shared with the server
	NoUpload bool
	// AutoStart indicates the app was intentionally started and should be
	// restored the next time the phelix monitor daemon launches (e.g. after a
	// machine reboot). Set to true whenever the app is started, cleared on an
	// intentional StopApplication. Persisted in apps.json as "auto_start".
	AutoStart bool

	// Process holds the process-supervision configuration for this app
	// (start/stop commands, resource limits, auto-restart policy).
	Process AppProcessConfig
	// Networking holds the network exposure configuration for this app
	// (listen port, TLS, proxy, CORS, rate limiting).
	Networking AppNetworkingConfig
	// Logging holds the log-capture configuration for this app
	// (format, levels, rotation).
	Logging AppLoggingConfig
	// Storage holds the disk-layout configuration for this app
	// (volumes, data dir, temp dir).
	Storage AppStorageConfig
}

// AppProcessConfig describes how Phelix launches and supervises a managed app.
type AppProcessConfig struct {
	// WorkingDir is the directory the app runs in (defaults to app.Directory).
	WorkingDir string `json:"working_dir,omitempty"`
	// Executable is the path to the app binary (defaults to app_<id> in Directory).
	Executable string `json:"executable,omitempty"`
	// StartCommand is the shell command used to start the app (empty => launch Executable directly).
	StartCommand string `json:"start_command,omitempty"`
	// StopCommand is the shell command used to stop the app (empty => SIGTERM).
	StopCommand string `json:"stop_command,omitempty"`
	// MaxCPUPercent caps CPU usage as a percentage (0 => unlimited).
	MaxCPUPercent int `json:"max_cpu_percent,omitempty"`
	// MaxMemoryMB caps resident memory in MiB (0 => unlimited).
	MaxMemoryMB int64 `json:"max_memory_mb,omitempty"`
	// MaxOpenFiles caps the number of open file descriptors (0 => OS default).
	MaxOpenFiles int64 `json:"max_open_files,omitempty"`
	// MaxProcesses caps the number of child processes (0 => unlimited).
	MaxProcesses int64 `json:"max_processes,omitempty"`
	// AutoRestart indicates the app should be restarted automatically if it exits.
	AutoRestart bool `json:"auto_restart,omitempty"`
	// CrashLoopBackoff is the seconds to wait before restarting after a crash (0 => default).
	CrashLoopBackoff int `json:"crash_loop_backoff,omitempty"`
	// GracefulShutdown is the seconds to wait for graceful shutdown before SIGKILL (0 => default).
	GracefulShutdown int `json:"graceful_shutdown,omitempty"`
	// MaxRestartAttempts is the maximum number of auto-restarts before giving up (0 => unlimited).
	MaxRestartAttempts int `json:"max_restart_attempts,omitempty"`
	// RestartDelayMs is the delay before an auto-restart, in milliseconds (0 => default).
	RestartDelayMs int `json:"restart_delay_ms,omitempty"`
}

// AppNetworkingConfig describes how a managed app is exposed to the network.
type AppNetworkingConfig struct {
	// ListenPort is the port the app listens on (defaults to app.Port).
	ListenPort int `json:"listen_port,omitempty"`
	// BindAddress is the address the app binds to (empty => 0.0.0.0).
	BindAddress string `json:"bind_address,omitempty"`
	// PublicDomain is the public domain/URL the app is served under (e.g. myapp.example.com).
	PublicDomain string `json:"public_domain,omitempty"`
	// BasePath is the URL path prefix the app is served under (e.g. /myapp).
	BasePath string `json:"base_path,omitempty"`
	// TLSEnabled indicates the app is served over TLS.
	TLSEnabled bool `json:"tls_enabled,omitempty"`
	// CertPath is the path to the TLS certificate (when TLSEnabled).
	CertPath string `json:"cert_path,omitempty"`
	// KeyPath is the path to the TLS private key (when TLSEnabled).
	KeyPath string `json:"key_path,omitempty"`
	// ProxyEnabled indicates the app is fronted by the phelix reverse proxy.
	ProxyEnabled bool `json:"proxy_enabled,omitempty"`
	// CORSEnabled indicates CORS headers are served.
	CORSEnabled bool `json:"cors_enabled,omitempty"`
	// AllowedOrigins is the list of CORS-allowed origins (when CORSEnabled).
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// RateLimitEnabled indicates per-client rate limiting is enabled.
	RateLimitEnabled bool `json:"rate_limit_enabled,omitempty"`
	// RateLimitRPS is the per-client request rate limit in requests/second (0 => default).
	RateLimitRPS int `json:"rate_limit_rps,omitempty"`
}

// AppLoggingConfig describes how a managed app's output is captured.
type AppLoggingConfig struct {
	// JSONLogs indicates logs are emitted as JSON.
	JSONLogs bool `json:"json_logs,omitempty"`
	// PersistentLogs indicates logs are persisted to disk.
	PersistentLogs bool `json:"persistent_logs,omitempty"`
	// LogLevel is the minimum log level captured (debug, info, warn, error).
	LogLevel string `json:"log_level,omitempty"`
	// LogFilePath is the path to the app's combined (stdout+stderr) log file.
	LogFilePath string `json:"log_file_path,omitempty"`
	// StderrFilePath is the path to a separate stderr log file (empty => combined).
	StderrFilePath string `json:"stderr_file_path,omitempty"`
	// LogFormat is the log line format (plain, json, ...).
	LogFormat string `json:"log_format,omitempty"`
	// RotationEnabled indicates the log files are rotated.
	RotationEnabled bool `json:"rotation_enabled,omitempty"`
	// RotationMaxSizeMB is the per-file rotation threshold in MiB (0 => default).
	RotationMaxSizeMB int `json:"rotation_max_size_mb,omitempty"`
	// RotationMaxFiles is the number of rotated files retained (0 => default).
	RotationMaxFiles int `json:"rotation_max_files,omitempty"`
	// RotationCompress indicates rotated files are gzip-compressed.
	RotationCompress bool `json:"rotation_compress,omitempty"`
}

// AppStorageConfig describes where a managed app stores its data on disk.
type AppStorageConfig struct {
	// Volumes is the list of volume mounts available to the app.
	Volumes []string `json:"volumes,omitempty"`
	// DataDir is the app's data directory (empty => default).
	DataDir string `json:"data_dir,omitempty"`
	// TempDir is the app's temp directory (empty => system temp).
	TempDir string `json:"temp_dir,omitempty"`
}

// AppStatus represents the current status of an application
type AppStatus struct {
	ID          string
	Name        string
	Status      string
	PID         int
	Uptime      string
	RAMUsage    uint64
	CPUUsage    float64
	BuildStatus string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Language    string
}

// AppListItem represents a simplified view of an application for listing
type AppListItem struct {
	ID          string
	Name        string
	Directory   string
	Status      string
	PID         int
	Port        int
	Uptime      string
	BuildStatus string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Language    string

	// Configuration groups, carried so monitoring/upload consumers can report
	// them without leaking back into the terminal list output.
	Process    AppProcessConfig
	Networking AppNetworkingConfig
	Logging    AppLoggingConfig
	Storage    AppStorageConfig
}
