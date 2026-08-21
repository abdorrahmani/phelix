package server

import "time"

// Info represents the server's basic information
type Info struct {
	ID            string    `json:"id"`
	AgentID       string    `json:"agent_id"`
	Hostname      string    `json:"hostname"`
	IPv4          string    `json:"ip_v4"`
	IPv6          string    `json:"ip_v6"`
	OSType        string    `json:"os_type"`
	OSFull        string    `json:"os_full"`
	CPUInfo       string    `json:"cpu_info"`
	NetworkIfaces []string  `json:"network_ifaces"`
	TotalMemory   int64     `json:"total_memory"`
	TotalCPUCores int       `json:"total_cpu_cores"`
	TotalStorage  int64     `json:"total_storage"`
	Uptime        string    `json:"uptime"`
	LastReboot    time.Time `json:"last_reboot"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	Region        string    `json:"region"`
	Architecture  string    `json:"architecture"`
	KernelVersion string    `json:"kernel_version"`
	SwapTotal     int64     `json:"swap_total"`

	// Server-settings groups (the server's real configuration, auto-detected
	// from the host), populated when settings detection has run; nil otherwise.
	// They are sent to the backend inside ServerInfo (fields
	// connection/alert/security).
	Connection *ServerConnection `json:"connection,omitempty"`
	Alert      *ServerAlert      `json:"alert,omitempty"`
	Security   *ServerSecurity   `json:"security,omitempty"`
}

// Metrics represents the server's current metrics
type Metrics struct {
	ServerID         string    `json:"server_id"`
	UsedMemory       uint64    `json:"used_memory"`
	FreeMemory       uint64    `json:"free_memory"`
	MemoryPercent    float64   `json:"memory_percent"`
	CPUUsagePercent  float64   `json:"cpu_usage_percent"`
	UsedStorage      uint64    `json:"used_storage"`
	FreeStorage      uint64    `json:"free_storage"`
	StoragePercent   float64   `json:"storage_percent"`
	Timestamp        time.Time `json:"timestamp"`
	NetworkIn        uint64    `json:"network_in"`
	NetworkOut       uint64    `json:"network_out"`
	LoadAvg1min      float64   `json:"load_avg_1min"`
	LoadAvg5min      float64   `json:"load_avg_5min"`
	LoadAvg15min     float64   `json:"load_avg_15min"`
	SwapUsed         uint64    `json:"swap_used"`
	SwapFree         uint64    `json:"swap_free"`
	SwapPercent      float64   `json:"swap_percent"`
	RunningProcesses uint32    `json:"running_processes"`
}
