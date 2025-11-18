package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/abdorrahmani/gophel/internal/network"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/mem"
)

var (
	serverID     string
	serverInfo   *Info
	serverIDFile string
)

func init() {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE") // For Windows
	}
	serverIDFile = filepath.Join(homeDir, ".gophel", "server_id")
}

// Initialize initializes the server information
func Initialize() error {
	// Load or generate server ID
	if err := loadOrGenerateServerID(); err != nil {
		return fmt.Errorf("failed to load/generate server ID: %w", err)
	}

	// Get hostname
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}

	// Get IP addresses
	ipv4List, ipv6List, err := network.GetPublicIPs()
	if err != nil {
		return fmt.Errorf("failed to get public IPs: %w", err)
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

	serverInfo = &Info{
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

// generateServerID generates a new server ID base on host name and mac address
func generateServerID() (string, error) {
	// 1. Try machine-id
	if id, err := readMachineID(); err != nil {
		return id, err
	}

	// 2. Try SMBIOS product_uuid
	if id, err := readSMBISOUUID(); err != nil {
		return id, err
	}

	// 3. Try CPU ID via dmidecode
	if id, err := readCPUId(); err != nil {
		return id, err
	}

	// 4. Fallback (hostname + MAC)
	return fallbackHash()
}

func readMachineID() (string, error) {
	data, err := os.ReadFile("/etc/machine-id")
	if err != nil || len(data) < 5 {
		return "", fmt.Errorf("machine-id not found")
	}
	return strings.TrimSpace(string(data)), nil
}

func readSMBISOUUID() (string, error) {
	data, err := os.ReadFile("/sys/class/dmi/id/product_uuid")
	if err != nil || len(data) < 5 {
		return "", fmt.Errorf("smb-iso-uuid not found")
	}
	return strings.TrimSpace(string(data)), nil
}

func readCPUId() (string, error) {
	out, err := exec.Command("dmidecode", "-t", "processor").Output()
	if err != nil {
		return "", err
	}

	re := regexp.MustCompile(`ID:\s*([0-9A-Fa-f]+)`)
	match := re.FindStringSubmatch(string(out))
	if len(match) < 2 {
		return "", fmt.Errorf("cpu id not found")
	}
	return match[1], nil
}

func fallbackHash() (string, error) {
	hostname, _ := os.Hostname()

	ifaces, _ := net.Interfaces()
	var mac string
	for _, iface := range ifaces {
		if len(iface.HardwareAddr) > 0 {
			mac = iface.HardwareAddr.String()
			break
		}
	}

	combined := hostname + mac
	h := sha256.Sum256([]byte(combined))
	return hex.EncodeToString(h[:]), nil
}

// loadOrGenerateServerID loads the server ID from file or generates a new one
func loadOrGenerateServerID() error {
	// Create .gophel directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(serverIDFile), 0755); err != nil {
		return fmt.Errorf("⚠ Failed to create .gophel directory: %w", err)
	}

	// Try to read existing server ID
	data, err := os.ReadFile(serverIDFile)
	if err == nil {
		serverID = string(data)
		return nil
	}

	// If file doesn't exist or can't be read, generate new ID
	if os.IsNotExist(err) {
		serverID, err = generateServerID()
		if err != nil {
			return err
		}

		if err := os.WriteFile(serverIDFile, []byte(serverID), 0600); err != nil {
			return fmt.Errorf("⚠ Failed to save server ID: %w", err)
		}
		return nil
	}

	return fmt.Errorf("⚠ Failed to read server ID file: %w", err)
}

// GetServerInfo returns the server information
func GetServerInfo() *Info {
	return serverInfo
}

// GetServerID returns the server ID
func GetServerID() string {
	return serverID
}

// CollectMetrics collects current server metrics
func CollectMetrics() (*Metrics, error) {
	// Get memory info
	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("⚠ Failed to get memory info: %w", err)
	}

	// Get CPU usage
	cpuPercent, err := cpu.Percent(time.Second, false)
	if err != nil {
		return nil, fmt.Errorf("⚠ Failed to get CPU usage: %w", err)
	}

	// Get disk info
	diskInfo, err := disk.Usage("/")
	if err != nil {
		return nil, fmt.Errorf("⚠ Failed to get disk info: %w", err)
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

	return &Metrics{
		ServerID:        serverID,
		UsedMemory:      memInfo.Used,
		FreeMemory:      memInfo.Free,
		CPUUsagePercent: cpuPercent[0],
		UsedStorage:     diskInfo.Used,
		FreeStorage:     diskInfo.Free,
		Timestamp:       time.Now(),
	}, nil
}
