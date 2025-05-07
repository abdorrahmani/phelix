package server

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/mem"
)

// ServerInfo represents the server's basic information
type ServerInfo struct {
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

// ServerMetrics represents the server's current metrics
type ServerMetrics struct {
	ServerID        string    `json:"server_id"`
	UsedMemory      uint64    `json:"used_memory"`
	FreeMemory      uint64    `json:"free_memory"`
	CPUUsagePercent float64   `json:"cpu_usage_percent"`
	UsedStorage     uint64    `json:"used_storage"`
	FreeStorage     uint64    `json:"free_storage"`
	Timestamp       time.Time `json:"timestamp"`
}

var (
	serverID   string
	serverInfo *ServerInfo
)

// Initialize initializes the server information
func Initialize() error {
	// Generate server ID if not exists
	if serverID == "" {
		serverID = uuid.New().String()
	}

	// Get hostname
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}

	// Get IP addresses
	var ipv4List, ipv6List []string
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				if ipnet.IP.To4() != nil {
					ipv4List = append(ipv4List, ipnet.IP.String())
				} else {
					ipv6List = append(ipv6List, ipnet.IP.String())
				}
			}
		}
	}

	// Convert IP lists to comma-separated strings
	ipv4 := strings.Join(ipv4List, ",")
	ipv6 := strings.Join(ipv6List, ",")

	// Get CPU info
	cpuInfo, err := cpu.Info()
	if err != nil {
		return fmt.Errorf("failed to get CPU info: %w", err)
	}
	cpuInfoStr := ""
	if len(cpuInfo) > 0 {
		cpuInfoStr = cpuInfo[0].ModelName
	}

	// Get memory info
	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return fmt.Errorf("failed to get memory info: %w", err)
	}

	// Get disk info
	diskInfo, err := disk.Usage("/")
	if err != nil {
		return fmt.Errorf("failed to get disk info: %w", err)
	}

	// Get OS info
	hostInfo, err := host.Info()
	if err != nil {
		return fmt.Errorf("failed to get host info: %w", err)
	}

	// Get uptime and last reboot
	uptime := time.Duration(hostInfo.Uptime) * time.Second
	lastReboot := time.Now().Add(-uptime)

	// Format uptime string
	days := int(uptime.Hours() / 24)
	hours := int(uptime.Hours()) % 24
	minutes := int(uptime.Minutes()) % 60
	uptimeStr := fmt.Sprintf("%dd %dh %dm", days, hours, minutes)

	// Get OS full details
	osFull := fmt.Sprintf("%s,%s %s", hostInfo.OS, hostInfo.Platform, hostInfo.PlatformVersion)

	// Determine server status based on system metrics
	status := "healthy"
	if memInfo.UsedPercent > 90 || diskInfo.UsedPercent > 90 {
		status = "warning"
	}
	if memInfo.UsedPercent > 95 || diskInfo.UsedPercent > 95 {
		status = "critical"
	}

	serverInfo = &ServerInfo{
		ID:            serverID,
		Hostname:      hostname,
		IPv4:          ipv4,
		IPv6:          ipv6,
		OSType:        hostInfo.OS,
		OSFull:        osFull,
		CPUInfo:       cpuInfoStr,
		TotalMemory:   int64(memInfo.Total),
		TotalCPUCores: runtime.NumCPU(),
		TotalStorage:  int64(diskInfo.Total),
		Uptime:        uptimeStr,
		LastReboot:    lastReboot,
		Status:        status,
		CreatedAt:     time.Now(),
	}

	return nil
}

// GetServerInfo returns the server information
func GetServerInfo() *ServerInfo {
	return serverInfo
}

// GetServerID returns the server ID
func GetServerID() string {
	return serverID
}

// CollectMetrics collects current server metrics
func CollectMetrics() (*ServerMetrics, error) {
	// Get memory info
	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("failed to get memory info: %w", err)
	}

	// Get CPU usage
	cpuPercent, err := cpu.Percent(time.Second, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get CPU usage: %w", err)
	}

	// Get disk info
	diskInfo, err := disk.Usage("/")
	if err != nil {
		return nil, fmt.Errorf("failed to get disk info: %w", err)
	}

	// Update server status based on current metrics
	if serverInfo != nil {
		status := "healthy"
		if memInfo.UsedPercent > 90 || diskInfo.UsedPercent > 90 {
			status = "warning"
		}
		if memInfo.UsedPercent > 95 || diskInfo.UsedPercent > 95 {
			status = "critical"
		}
		serverInfo.Status = status
	}

	return &ServerMetrics{
		ServerID:        serverID,
		UsedMemory:      memInfo.Used,
		FreeMemory:      memInfo.Free,
		CPUUsagePercent: cpuPercent[0],
		UsedStorage:     diskInfo.Used,
		FreeStorage:     diskInfo.Free,
		Timestamp:       time.Now(),
	}, nil
}
