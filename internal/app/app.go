package app

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shirou/gopsutil/process"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type AppManagerInterface interface {
	GenerateAppID() string
	StartApplication(id string)
	StopApplication(id string)
	RestartApplication(id string) error
	StatusApplication(id string) (AppStatus, error)
	ListApplications() []struct {
		ID     string
		Status string
		PID    int
		Uptime string
	}
	SaveState() error
	LoadState() error
}

type AppInfo struct {
	ID     string
	Cmd    *exec.Cmd
	PID    int
	Status string
	Start  time.Time
}

type AppStatus struct {
	ID       string
	Status   string
	PID      int
	Uptime   string
	RAMUsage uint64  // in bytes
	CPUUsage float64 // in percentage
}

type AppManager struct {
	Apps map[string]*AppInfo
	Lock sync.Mutex
}

var Manager AppManagerInterface = &AppManager{
	Apps: make(map[string]*AppInfo),
}

const stateFile = "/var/lib/gophel/apps.json"

// GenerateAppID generates a unique ID for an application.
func (m *AppManager) GenerateAppID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// StartApplication starts an application by its ID.
func (m *AppManager) StartApplication(id string) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if app, exists := m.Apps[id]; exists && app.Status == "running" {
		fmt.Printf("Application %s is already running\n", id)
		return
	}
	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		fmt.Println("Start failed:", err)
		return
	}
	m.Apps[id] = &AppInfo{
		ID:     id,
		Cmd:    cmd,
		PID:    cmd.Process.Pid,
		Status: "running",
		Start:  time.Now(),
	}
	fmt.Printf("Application %s started successfully\n", id)
	if err := m.SaveState(); err != nil {
		fmt.Println("Failed to save state:", err)
	}
}

// StopApplication stops an application by its ID.
func (m *AppManager) StopApplication(id string) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if app, exists := m.Apps[id]; exists && app.Status == "running" {
		if err := app.Cmd.Process.Kill(); err != nil {
			fmt.Println("Failed to stop application:", err)
			return
		}
		app.Status = "stopped"
		if err := m.SaveState(); err != nil {
			fmt.Println("Failed to save state:", err)
		}
	} else {
		fmt.Printf("Application %s not found or not running\n", id)
	}
}

// RestartApplication restarts an application by its ID.
func (m *AppManager) RestartApplication(id string) error {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if app, exists := m.Apps[id]; exists && app.Status == "running" {
		if err := app.Cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to stop application %s: %v", id, err)
		}
		app.Status = "stopped"
	} else {
		fmt.Printf("Application %s was not running, starting it now\n", id)
	}

	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to restart application %s: %v", id, err)
	}

	m.Apps[id] = &AppInfo{
		ID:     id,
		Cmd:    cmd,
		PID:    cmd.Process.Pid,
		Status: "running",
		Start:  time.Now(),
	}
	if err := m.SaveState(); err != nil {
		fmt.Println("Failed to save state:", err)
	}
	return nil
}

// StatusApplication returns the status of an application by its ID.
func (m *AppManager) StatusApplication(id string) (AppStatus, error) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return AppStatus{}, fmt.Errorf("failed to load state: %v", err)
	}
	app, exists := m.Apps[id]
	if !exists {
		return AppStatus{}, errors.New("application not found")
	}
	if app.Cmd != nil && app.Cmd.ProcessState != nil && app.Cmd.ProcessState.Exited() {
		app.Status = "stopped"
		if err := m.SaveState(); err != nil {
			fmt.Println("Failed to save state:", err)
		}
	}

	// Calculate uptime
	var uptime string
	if app.Status == "running" {
		duration := time.Since(app.Start)
		uptime = FormatDuration(duration)
	} else {
		uptime = "N/A"
	}

	// Get RAM and CPU usage
	var ramUsage uint64
	var cpuUsage float64
	if app.Status == "running" {
		p, err := process.NewProcess(int32(app.PID))
		if err == nil {
			memInfo, _ := p.MemoryInfo()
			if memInfo != nil {
				ramUsage = memInfo.RSS
			}
			cpuUsage, _ = p.CPUPercent()
		}
	}

	return AppStatus{
		ID:       id,
		Status:   app.Status,
		PID:      app.PID,
		Uptime:   uptime,
		RAMUsage: ramUsage,
		CPUUsage: cpuUsage,
	}, nil
}

// ListApplications returns a list of all applications with their details.
func (m *AppManager) ListApplications() []struct {
	ID     string
	Status string
	PID    int
	Uptime string
} {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		fmt.Println("Failed to load state:", err)
	}

	var appList []struct {
		ID     string
		Status string
		PID    int
		Uptime string
	}

	for id, app := range m.Apps {
		if app.Cmd != nil && app.Cmd.ProcessState != nil && app.Cmd.ProcessState.Exited() {
			app.Status = "stopped"
			if err := m.SaveState(); err != nil {
				fmt.Println("Failed to save state:", err)
			}
		}

		var uptime string
		if app.Status == "running" {
			duration := time.Since(app.Start)
			uptime = FormatDuration(duration)
		} else {
			uptime = "N/A"
		}

		appList = append(appList, struct {
			ID     string
			Status string
			PID    int
			Uptime string
		}{
			ID:     id,
			Status: app.Status,
			PID:    app.PID,
			Uptime: uptime,
		})
	}
	return appList
}

// SaveState saves the current app state to a file.
func (m *AppManager) SaveState() error {
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return err
	}

	// Simplified state: only save ID, PID, Status, and Start time
	type SavedApp struct {
		ID     string    `json:"id"`
		PID    int       `json:"pid"`
		Status string    `json:"status"`
		Start  time.Time `json:"start"`
	}
	savedApps := make(map[string]SavedApp)
	for id, app := range m.Apps {
		savedApps[id] = SavedApp{
			ID:     id,
			PID:    app.PID,
			Status: app.Status,
			Start:  app.Start,
		}
	}

	data, err := json.MarshalIndent(savedApps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

// LoadState loads the app state from a file.
func (m *AppManager) LoadState() error {
	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return nil // No state file yet, start fresh
	} else if err != nil {
		return err
	}

	type SavedApp struct {
		ID     string    `json:"id"`
		PID    int       `json:"pid"`
		Status string    `json:"status"`
		Start  time.Time `json:"start"`
	}
	savedApps := make(map[string]SavedApp)
	if err := json.Unmarshal(data, &savedApps); err != nil {
		return err
	}

	for id, saved := range savedApps {
		// Only load, don’t restart processes; assume they’re gone unless running
		m.Apps[id] = &AppInfo{
			ID:     id,
			PID:    saved.PID,
			Status: saved.Status,
			Start:  saved.Start,
			Cmd:    nil,
		}
	}
	return nil
}

func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	return fmt.Sprintf("%02dh %02dm %02ds", h, m, s)
}
