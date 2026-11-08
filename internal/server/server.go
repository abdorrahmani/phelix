package server

import (
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/network"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/host"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	gopsutilnet "github.com/shirou/gopsutil/net"
)

var (
	agentID      string
	serverInfo   *Info
	serverInfoMu sync.RWMutex
	agentIDFile  string

	// agentIDMu serializes agent-ID file reads and writes. Without it, two
	// goroutines racing to initialize the runtime could both observe a
	// missing file, generate two different IDs, and only the last write
	// would survive.
	agentIDMu sync.Mutex
)

func init() {
	agentIDFile = filepath.Join(agentDataDir(), "agent-id")
}

// agentDataDir is the persistent state directory for the Phelix runtime.
// Docker deployments use /var/lib/phelix so mounting that path preserves the
// runtime identity across container recreation. Local installations retain
// their existing per-user state unless PHELIX_DATA_DIR overrides it.
func agentDataDir() string {
	if dir := os.Getenv("PHELIX_DATA_DIR"); dir != "" {
		return dir
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "/var/lib/phelix"
	}
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE")
	}
	return filepath.Join(homeDir, ".phelix")
}

// DataDir returns the persistent Phelix runtime data directory. Runtime
// subsystems use this single resolver so identity, command ledgers, and other
// durable state honor PHELIX_DATA_DIR and container storage consistently.
func DataDir() string { return agentDataDir() }

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
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}

	var n uint32
	for _, e := range entries {
		name := e.Name()
		if name[0] >= '0' && name[0] <= '9' {
			n++
		}
	}

	return n, nil
}

// Initialize initializes the server information
func Initialize() error {
	id, err := loadOrGenerateAgentID()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to load/generate agent ID", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get hostname", err)
	}

	ipv4List, ipv6List, err := network.GetPublicIPs()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "failed to get public IPs", err)
	}

	ipv4 := strings.Join(ipv4List, ",")
	ipv6 := strings.Join(ipv6List, ",")

	cpuInfo, err := cpu.Info()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get CPU info", err)
	}
	cpuInfoStr := ""
	if len(cpuInfo) > 0 {
		cpuInfoStr = cpuInfo[0].ModelName
	}

	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get memory info", err)
	}

	diskInfo, err := disk.Usage("/")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get disk info", err)
	}

	hostInfo, err := host.Info()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get host info", err)
	}

	uptime := time.Duration(hostInfo.Uptime) * time.Second
	lastReboot := time.Now().Add(-uptime)

	days := int(uptime.Hours() / 24)
	hours := int(uptime.Hours()) % 24
	minutes := int(uptime.Minutes()) % 60
	uptimeStr := fmt.Sprintf("%dd %dh %dm", days, hours, minutes)

	osFull := fmt.Sprintf("%s,%s %s", hostInfo.OS, hostInfo.Platform, hostInfo.PlatformVersion)

	region := getServerRegion()
	arch := getArchitecture()
	kernelVersion := getKernelVersion()

	swapInfo, err := mem.SwapMemory()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeServer, "failed to get swap info", err)
	}

	ifaces, _ := net.Interfaces()
	var ifaceList []string
	for _, iface := range ifaces {
		mac := iface.HardwareAddr.String()
		if mac == "" {
			mac = "<no-mac>"
		}
		ifaceList = append(ifaceList, fmt.Sprintf("%s(%s)", iface.Name, mac))
	}

	status := "healthy"
	if memInfo.UsedPercent > 90 || diskInfo.UsedPercent > 90 {
		status = "warning"
	}
	if memInfo.UsedPercent > 95 || diskInfo.UsedPercent > 95 {
		status = "critical"
	}

	info := &Info{
		ID:            id,
		AgentID:       id,
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

	if s := EnsureSettings(); s != nil {
		info.Connection = &s.Connection
		info.Alert = &s.Alert
		info.Security = &s.Security
	}
	serverInfoMu.Lock()
	serverInfo = info
	serverInfoMu.Unlock()

	return nil
}

// generateAgentID returns an RFC 4122 version 4 UUID. It is random rather
// than derived from host data, so every Phelix runtime has its own identity.
func generateAgentID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b), nil
}

var agentIDFormat = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// validAgentIDFormat reports whether id is a well-formed version 4 UUID, the
// only format loadOrGenerateAgentID ever writes.
func validAgentIDFormat(id string) bool {
	return agentIDFormat.MatchString(id)
}

// formatUUID renders 16 random bytes as an RFC 4122 UUID string.
func formatUUID(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// loadOrGenerateAgentID loads the persistent agent ID or creates it once and
// returns it. The read-check-write path is serialized so concurrent
// initializations cannot mint two different IDs for one data directory; the
// write is crash-safe (write a temp file, fsync, then atomically rename over
// the final path) so a crash mid-write never leaves a truncated agent-id.

func loadOrGenerateAgentID() (string, error) {
	agentIDMu.Lock()
	defer agentIDMu.Unlock()

	if err := os.MkdirAll(filepath.Dir(agentIDFile), 0755); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to create Phelix data directory", err)
	}

	data, err := os.ReadFile(agentIDFile)
	if err == nil {
		agentID = strings.TrimSpace(string(data))
		if agentID == "" {
			return "", phelixerr.New(phelixerr.CodeConfiguration, "agent ID file is empty")
		}
		if !validAgentIDFormat(agentID) {
			return "", phelixerr.New(phelixerr.CodeConfiguration, "agent ID file contains an invalid ID")
		}
		return agentID, nil
	}

	if !os.IsNotExist(err) {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read agent ID file", err)
	}

	id, err := generateAgentID()
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeServer, "failed to generate agent ID", err)
	}

	tmpFile := agentIDFile + ".tmp"
	f, err := os.OpenFile(tmpFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to create agent ID temp file", err)
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		_ = os.Remove(tmpFile)
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to write agent ID temp file", err)
	}

	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmpFile)
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to fsync agent ID temp file", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpFile)
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to close agent ID temp file", err)
	}
	if err := os.Rename(tmpFile, agentIDFile); err != nil {
		_ = os.Remove(tmpFile)
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to replace agent ID file", err)
	}

	agentID = id
	return agentID, nil
}

// GetServerInfo returns the server information
func GetServerInfo() *Info {
	serverInfoMu.RLock()
	defer serverInfoMu.RUnlock()
	if serverInfo == nil {
		return nil
	}
	info := *serverInfo
	return &info
}

// GetAgentID returns the persistent identity of this Phelix runtime.
func GetAgentID() string {
	agentIDMu.Lock()
	defer agentIDMu.Unlock()
	return agentID
}

// GetServerID is retained for existing message fields. Its value is the
// agent ID, not a host-derived machine ID. Backends must resolve it by
// agent_id to their internal servers.id.
func GetServerID() string {
	return GetAgentID()
}

// metricsCache holds the latest host-wide metrics snapshot produced by the
// background sampler (StartMetricsSampler). Readers get a copy of it instead
// of measuring the host themselves, so reading metrics never blocks.
var (
	metricsCacheMu sync.RWMutex
	metricsCache   *Metrics
)

var startMetricsSamplerOnce sync.Once

// StartMetricsSampler starts the background goroutine that refreshes the
// host-wide metrics snapshot every interval. It is idempotent: later calls
// (every monitor stream (re)connection) are no-ops.
//
// This exists because the only expensive read in the snapshot is CPU:
// cpu.Percent with a positive interval blocks for that interval while it
// measures, and the monitor stream used to pay that second on its send loop
// for every server tick — which is why server metrics were slowed to a 30s
// cadence. With the sampler the measurement happens continuously on its own
// goroutine and sendServerTick reads a ready snapshot, so the cadence is
// free to match the app-metrics tick.
func StartMetricsSampler(interval time.Duration) {
	startMetricsSamplerOnce.Do(func() {
		go runMetricsSampler(interval)
	})
}

// runMetricsSampler refreshes the cached snapshot once per interval. The
// previous sample is kept on a failed read: a transient syscall error should
// not blank the dashboard, and the next tick overwrites it anyway.
func runMetricsSampler(interval time.Duration) {
	// Prime gopsutil's counter baseline: the first cpu.Percent(0) call
	// reports usage since process start, which is not a meaningful sample.
	// Discarding it makes the first cached sample the delta across the first
	// real interval instead.
	_, _ = cpu.Percent(0, false)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		m, err := collectMetricsOnce()
		if err != nil {
			logs.Warning("server", "metrics sampler read failed, keeping previous sample: %v", err)
			continue
		}
		metricsCacheMu.Lock()
		metricsCache = m
		metricsCacheMu.Unlock()
	}
}

// CollectMetrics returns the current server metrics. The fast path is the
// background sampler's latest snapshot (see StartMetricsSampler); when the
// sampler has not been started, or its first sample has not landed yet, the
// read happens synchronously so a caller never gets nil. That synchronous
// path is non-blocking too: cpu.Percent(0) reports the delta since the
// previous call rather than measuring for a fixed interval.
func CollectMetrics() (*Metrics, error) {
	metricsCacheMu.RLock()
	cached := metricsCache
	metricsCacheMu.RUnlock()
	if cached != nil {
		m := *cached
		return &m, nil
	}
	return collectMetricsOnce()
}

// collectMetricsOnce samples the host once. CPU is read incrementally
// (cpu.Percent(0)): the caller — the sampler loop, or a fallback read —
// provides the elapsed window, so this never blocks.
func collectMetricsOnce() (*Metrics, error) {
	memInfo, err := mem.VirtualMemory()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get memory info", err)
	}

	cpuPercent, err := cpu.Percent(0, false)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get CPU usage", err)
	}

	diskInfo, err := disk.Usage("/")
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get disk info", err)
	}

	networkIn, networkOut, err := getNetworkStats()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeNetwork, "failed to get network stats", err)
	}

	loadAvg1, loadAvg5, loadAvg15, err := getLoadAverage()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get load average", err)
	}

	swapInfo, err := mem.SwapMemory()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get swap info", err)
	}

	procCount, err := getProcessCount()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeServer, "failed to get process count", err)
	}

	serverInfoMu.Lock()
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
	serverInfoMu.Unlock()

	var swapPercent float64
	if swapInfo.Total > 0 {
		swapPercent = (float64(swapInfo.Used) / float64(swapInfo.Total)) * 100
	}

	return &Metrics{
		ServerID:         GetAgentID(),
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
