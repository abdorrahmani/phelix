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

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/network"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	gopsutilnet "github.com/shirou/gopsutil/net"
	"github.com/shirou/gopsutil/process"
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
	serverIDFile = filepath.Join(homeDir, ".phelix", "server_id")
}

// getNetworkStats returns total network bytes in and out across all interfaces
func getNetworkStats() (uint64, uint64, error) {
	var totalBytesIn, totalBytesOut uint64

	stats, err := gopsutilnet.IOCounters(true)
	if err != nil {
		return 0, 0, err
	}

	for _, stat := range stats {
		totalBytesIn += stat.BytesRecv
		totalBytesOut += stat.BytesSent
	}

	return totalBytesIn, totalBytesOut, nil
}

// getLoadAverage returns the system load average
func getLoadAverage() (float64, float64, float64, error) {
	avg, err := load.Avg()
	if err != nil {
		return 0, 0, 0, err
	}
	return avg.Load1, avg.Load5, avg.Load15, nil
}

// getProcessCount returns the number of running processes
func getProcessCount() (uint32, error) {
	processes, err := process.Processes()
	if err != nil {
		return 0, err
	}
	return uint32(len(processes)), nil
}

// Initialize initializes the server information
func Initialize() error {
	// Load or generate server ID
	if err := loadOrGenerateServerID(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to load/generate server ID", err)
	}

	// Get hostname
	hostname, err := os.Hostname()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get hostname", err)
	}

	// Get IP addresses
	ipv4List, ipv6List, err := network.GetPublicIPs()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "failed to get public IPs", err)
	}

	// Convert IP lists to comma-separated strings
	ipv4 := strings.Join(ipv4List, ",")
	ipv6 := strings.Join(ipv6List, ",")

	// Get CPU info
	cpuInfo, err := cpu.Info()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get CPU info", err)
	}
	cpuInfoStr := ""
	if len(cpuInfo) > 0 {
		cpuInfoStr = cpuInfo[0].ModelName
	}

	// Get memory info
	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get memory info", err)
	}

	// Get disk info
	diskInfo, err := disk.Usage("/")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get disk info", err)
	}

	// Get OS info
	hostInfo, err := host.Info()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get host info", err)
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

	// Get region, architecture, and kernel version
	region := getServerRegion()
	arch := getArchitecture()
	kernelVersion := getKernelVersion()

	// Get swap info
	swapInfo, err := mem.SwapMemory()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get swap info", err)
	}

	// Collect network interface names and MACs
	ifaces, _ := net.Interfaces()
	var ifaceList []string
	for _, iface := range ifaces {
		mac := iface.HardwareAddr.String()
		if mac == "" {
			mac = "<no-mac>"
		}
		ifaceList = append(ifaceList, fmt.Sprintf("%s(%s)", iface.Name, mac))
	}

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
		NetworkIfaces: ifaceList,
		TotalMemory:   int64(memInfo.Total),
		TotalCPUCores: runtime.NumCPU(),
		TotalStorage:  int64(diskInfo.Total),
		Uptime:        uptimeStr,
		LastReboot:    lastReboot,
		Status:        status,
		CreatedAt:     time.Now(),
		Region:        region,
		Architecture:  arch,
		KernelVersion: kernelVersion,
		SwapTotal:     int64(swapInfo.Total),
	}

	// Attach the effective server settings (the server's real configuration,
	// auto-detected and persisted across restarts). Detection is
	// sync.Once-guarded and never fails.
	if s := EnsureSettings(); s != nil {
		serverInfo.Connection = &s.Connection
		serverInfo.Alert = &s.Alert
		serverInfo.Security = &s.Security
	}

	return nil
}

type idReader func() (string, error)

// generateServerID generates a new server ID base on host name and mac address
func generateServerID() (string, error) {
	readers := []idReader{
		readMachineID,
		readSMBISOUUID,
		readCPUId,
		fallbackHash,
	}

	for _, r := range readers {
		if id, err := r(); err == nil && id != "" {
			return id, nil
		}
	}

	return "", fmt.Errorf("no suitable hardware ID found for server ID generation")
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
	// Create .phelix directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(serverIDFile), 0755); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to create .phelix directory", err)
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
			return phelixerr.Wrap(phelixerr.CodeServer, "failed to generate server ID", err)
		}

		if err := os.WriteFile(serverIDFile, []byte(serverID), 0600); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to save server ID", err)
		}
		return nil
	}

	return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read server ID file", err)
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
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get memory info", err)
	}

	// Get CPU usage
	cpuPercent, err := cpu.Percent(time.Second, false)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get CPU usage", err)
	}

	// Get disk info
	diskInfo, err := disk.Usage("/")
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get disk info", err)
	}

	// Get network stats
	networkIn, networkOut, err := getNetworkStats()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeNetwork, "failed to get network stats", err)
	}

	// Get load average
	loadAvg1, loadAvg5, loadAvg15, err := getLoadAverage()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get load average", err)
	}

	// Get swap info
	swapInfo, err := mem.SwapMemory()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get swap info", err)
	}

	// Get process count
	procCount, err := getProcessCount()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get process count", err)
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

	// Compute swap percent
	var swapPercent float64
	if swapInfo.Total > 0 {
		swapPercent = (float64(swapInfo.Used) / float64(swapInfo.Total)) * 100
	}

	return &Metrics{
		ServerID:         serverID,
		UsedMemory:       memInfo.Used,
		FreeMemory:       memInfo.Free,
		MemoryPercent:    memInfo.UsedPercent,
		CPUUsagePercent:  cpuPercent[0],
		UsedStorage:      diskInfo.Used,
		FreeStorage:      diskInfo.Free,
		StoragePercent:   diskInfo.UsedPercent,
		Timestamp:        time.Now(),
		NetworkIn:        networkIn,
		NetworkOut:       networkOut,
		LoadAvg1min:      loadAvg1,
		LoadAvg5min:      loadAvg5,
		LoadAvg15min:     loadAvg15,
		SwapUsed:         swapInfo.Used,
		SwapFree:         swapInfo.Free,
		SwapPercent:      swapPercent,
		RunningProcesses: procCount,
	}, nil
}
