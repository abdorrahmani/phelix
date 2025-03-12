package monitor

import (
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/abdorrahmani/gophel/internal/websocket"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/disk"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/process"
	"time"
)

// AppStats holds monitoring data for a single application.
type AppStats struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	PID       int     `json:"pid"`
	Status    string  `json:"status"`
	Uptime    string  `json:"uptime"`
	CPU       float64 `json:"cpu_usage"`
	RAM       uint64  `json:"ram_usage"`
	Logs      string  `json:"logs"` // Placeholder for logs
	Timestamp int64   `json:"timestamp"`
}

// SystemStats holds system-wide monitoring data.
type SystemStats struct {
	CPU       float64 `json:"cpu_usage"`
	RAM       uint64  `json:"ram_usage"`
	HDD       uint64  `json:"hdd_usage"`
	Timestamp int64   `json:"timestamp"`
}

func StartMonitoring() {
	go func() {
		for {
			// Collect and send system stats
			sysStats := collectSystemStats()
			websocket.SendStats("system", sysStats)

			// Collect and send stats for each app
			apps := app.Manager.ListApplications()
			for _, appInfo := range apps {
				stats := collectAppStats(appInfo)
				websocket.SendStats(appInfo.ID, stats)
			}

			time.Sleep(5 * time.Second)
		}
	}()
}

func collectSystemStats() SystemStats {
	c, _ := cpu.Percent(time.Second, false)
	m, _ := mem.VirtualMemory()
	d, _ := disk.Usage("/")

	return SystemStats{
		CPU:       c[0],
		RAM:       m.Used,
		HDD:       d.Used,
		Timestamp: time.Now().Unix(),
	}
}

func collectAppStats(appInfo struct {
	ID     string
	Name   string
	Status string
	PID    int
	Uptime string
}) AppStats {
	p, err := process.NewProcess(int32(appInfo.PID))
	if err != nil || appInfo.Status != "running" {
		return AppStats{
			ID:        appInfo.ID,
			Name:      appInfo.Name,
			PID:       appInfo.PID,
			Status:    appInfo.Status,
			Uptime:    appInfo.Uptime,
			CPU:       0,
			RAM:       0,
			Logs:      "N/A",
			Timestamp: time.Now().Unix(),
		}
	}

	cpuPercent, _ := p.CPUPercent()
	memInfo, _ := p.MemoryInfo()

	return AppStats{
		ID:        appInfo.ID,
		Name:      appInfo.Name,
		PID:       appInfo.PID,
		Status:    appInfo.Status,
		Uptime:    appInfo.Uptime,
		CPU:       cpuPercent,
		RAM:       memInfo.RSS,
		Logs:      "N/A", // Implement log collection if needed
		Timestamp: time.Now().Unix(),
	}
}
