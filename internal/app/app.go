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
	"syscall"
	"time"
)

type AppManagerInterface interface {
	GenerateAppID() string
	StartApplication(id string, port int) error
	StopApplication(id string) error
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
	Port   int
}

type AppStatus struct {
	ID       string
	Status   string
	PID      int
	Uptime   string
	RAMUsage uint64  // in bytes (will be converted to MB in status command)
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

// StartApplication starts an application by its ID with a specified port.
func (m *AppManager) StartApplication(id string, port int) error {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return fmt.Errorf("failed to load state: %v", err)
	}

	if app, exists := m.Apps[id]; exists && app.Status == "running" {
		return fmt.Errorf("application %s is already running on PID %d", id, app.PID)
	}

	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start failed: %v", err)
	}

	m.Apps[id] = &AppInfo{
		ID:     id,
		Cmd:    cmd,
		PID:    cmd.Process.Pid,
		Status: "running",
		Start:  time.Now(),
		Port:   port,
	}
	if err := m.SaveState(); err != nil {
		fmt.Println("Failed to save state:", err)
	}
	fmt.Printf("Application %s started successfully on port %d\n", id, port)
	return nil
}

// StopApplication stops an application by its ID.
func (m *AppManager) StopApplication(id string) error {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return fmt.Errorf("failed to load state: %v", err)
	}

	app, exists := m.Apps[id]
	if !exists {
		return fmt.Errorf("application %s not found", id)
	}
	if app.Status != "running" {
		return fmt.Errorf("application %s is not running", id)
	}

	if app.Cmd != nil && app.Cmd.Process != nil {
		if stdin, _ := app.Cmd.StdinPipe(); stdin != nil {
			stdin.Close()
		}

		if err := app.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
			fmt.Println("Failed to send SIGTERM:", err)
		}

		done := make(chan error, 1)
		go func() {
			done <- app.Cmd.Wait()
		}()

		select {
		case <-time.After(5 * time.Second):
			fmt.Println("Process did not stop gracefully, attempting SIGKILL")
			if err := app.Cmd.Process.Kill(); err != nil {
				return fmt.Errorf("failed to force kill process: %v", err)
			}
		case err := <-done:
			if err != nil {
				fmt.Println("Process exited with error:", err)
			}
		}
	}

	time.Sleep(2 * time.Second)
	p, err := process.NewProcess(int32(app.PID))
	if err == nil {
		alive, _ := p.IsRunning()
		if alive {
			fmt.Println("Process is still running, using system kill command")
			exec.Command("kill", "-9", fmt.Sprintf("%d", app.PID)).Run()
			time.Sleep(1 * time.Second)
		}
	}

	p, err = process.NewProcess(int32(app.PID))
	if err == nil {
		alive, _ := p.IsRunning()
		if alive {
			return fmt.Errorf("failed to stop application %s: process still running", id)
		}
	}

	app.Status = "stopped"
	app.Cmd = nil
	if err := m.SaveState(); err != nil {
		fmt.Println("Failed to save state:", err)
	}
	fmt.Printf("Application %s stopped successfully\n", id)
	return nil
}

// RestartApplication restarts an application by its ID.
func (m *AppManager) RestartApplication(id string) error {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return fmt.Errorf("failed to load state: %v", err)
	}

	app, exists := m.Apps[id]
	if !exists {
		return fmt.Errorf("application %s not found", id)
	}

	fmt.Printf("Restarting Application %s\n", id)

	if app.Status == "running" {
		if err := m.stopApplicationUnlocked(id); err != nil {
			return fmt.Errorf("failed to stop application %s: %v", id, err)
		}
	}

	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to restart application %s: %v", id, err)
	}

	app.Cmd = cmd
	app.PID = cmd.Process.Pid
	app.Status = "running"
	app.Start = time.Now()

	if err := m.SaveState(); err != nil {
		fmt.Println("Failed to save state:", err)
	}

	fmt.Printf("Application %s restarted successfully on port %d\n", id, app.Port)
	return nil
}

// stopApplicationUnlocked stops an application without locking (to prevent deadlock)
func (m *AppManager) stopApplicationUnlocked(id string) error {
	app, exists := m.Apps[id]
	if !exists {
		return fmt.Errorf("application %s not found", id)
	}

	if app.Status != "running" {
		return fmt.Errorf("application %s is not running", id)
	}

	if app.Cmd != nil && app.Cmd.Process != nil {
		if err := app.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
			fmt.Println("Failed to send SIGTERM:", err)
		}

		done := make(chan error, 1)
		go func() {
			done <- app.Cmd.Wait()
		}()

		select {
		case <-time.After(5 * time.Second):
			fmt.Println("Process did not stop gracefully, using SIGKILL")
			_ = app.Cmd.Process.Kill()
		case err := <-done:
			if err != nil {
				fmt.Println("Process exited with error:", err)
			}
		}
	}

	time.Sleep(1 * time.Second)
	p, err := process.NewProcess(int32(app.PID))
	if err == nil {
		if alive, _ := p.IsRunning(); alive {
			exec.Command("kill", "-9", fmt.Sprintf("%d", app.PID)).Run()
			time.Sleep(1 * time.Second)
		}
	}

	app.Status = "stopped"
	app.Cmd = nil
	if err := m.SaveState(); err != nil {
		fmt.Println("Failed to save state:", err)
	}

	fmt.Printf("Application %s stopped successfully\n", id)
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

	// Get RAM and CPU usage for running apps by querying the PID directly
	var ramUsage uint64
	var cpuUsage float64
	if app.Status == "running" {
		p, err := process.NewProcess(int32(app.PID))
		if err == nil {
			// Retry mechanism to ensure non-zero CPU usage
			for i := 0; i < 3; i++ {
				time.Sleep(200 * time.Millisecond) // Wait for process to stabilize
				memInfo, _ := p.MemoryInfo()
				if memInfo != nil {
					ramUsage = memInfo.RSS
				}
				cpuUsage, _ = p.CPUPercent()
				if cpuUsage > 0 {
					break // Exit retry if we get a non-zero value
				}
			}
		} else {
			// If process not found, mark as stopped
			app.Status = "stopped"
			if err := m.SaveState(); err != nil {
				fmt.Println("Failed to save state:", err)
			}
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
		Port   int       `json:"port"`
	}
	savedApps := make(map[string]SavedApp)
	for id, app := range m.Apps {
		savedApps[id] = SavedApp{
			ID:     id,
			PID:    app.PID,
			Status: app.Status,
			Start:  app.Start,
			Port:   app.Port,
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
		Port   int       `json:"port"`
	}
	savedApps := make(map[string]SavedApp)
	if err := json.Unmarshal(data, &savedApps); err != nil {
		return err
	}

	for id, saved := range savedApps {
		m.Apps[id] = &AppInfo{
			ID:     id,
			PID:    saved.PID,
			Status: saved.Status,
			Start:  saved.Start,
			Port:   saved.Port,
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
