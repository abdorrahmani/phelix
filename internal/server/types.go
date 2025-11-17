package server

import "time"

// Info represents the server's basic information
type Info struct {
	ID            string    `json:"id"`
	Hostname      string    `json:"hostname"`
	IPv4          string    `json:"ip_v4"`
	IPv6          string    `json:"ip_v6"`
	OSType        string    `json:"os_type"`
	OSFull        string    `json:"os_full"`
	CPUInfo       string    `json:"cpu_info"`
	TotalMemory   int64     `json:"total_memory"`
	TotalCPUCores int       `json:"total_cpu_cores"`
	TotalStorage  int64     `json:"total_storage"`
	Uptime        string    `json:"uptime"`
	LastReboot    time.Time `json:"last_reboot"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// Metrics represents the server's current metrics
type Metrics struct {
	ServerID        string    `json:"server_id"`
	UsedMemory      uint64    `json:"used_memory"`
	FreeMemory      uint64    `json:"free_memory"`
	CPUUsagePercent float64   `json:"cpu_usage_percent"`
	UsedStorage     uint64    `json:"used_storage"`
	FreeStorage     uint64    `json:"free_storage"`
	Timestamp       time.Time `json:"timestamp"`
}
