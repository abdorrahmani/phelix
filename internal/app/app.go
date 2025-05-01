package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/process"
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
}

// AppListItem represents a simplified view of an application for listing
type AppListItem struct {
	ID          string
	Name        string
	Status      string
	PID         int
	Uptime      string
	BuildStatus string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AppManager manages the lifecycle of applications
type AppManager struct {
	Apps   map[string]*AppInfo
	Lock   sync.Mutex
	NextID uint
}

var Manager AppManagerInterface = &AppManager{
	Apps:   make(map[string]*AppInfo),
	NextID: 1,
}

const (
	stateFile = "/var/lib/gophel/apps.json"
	logDir    = "/var/log/gophel"
)

// GenerateAppID generates a sequential numeric ID for an application
func (m *AppManager) GenerateAppID() string {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	id := fmt.Sprintf("%d", m.NextID)
	m.NextID++
	return id
}

// StartApplication starts an application with the given parameters
func (m *AppManager) StartApplication(id string, port int, name string) error {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return fmt.Errorf("failed to load state: %v", err)
	}

	if app, exists := m.Apps[id]; exists {
		if app.Status == "running" {
			return fmt.Errorf("application '%s' (ID: %s) is already running on PID %d", app.Name, id, app.PID)
		}
		if app.Name != "" {
			name = app.Name
		}
	}

	if err := m.ensureLogDirectory(); err != nil {
		return err
	}

	logFile := filepath.Join(logDir, fmt.Sprintf("%s.log", id))
	if err := m.startApplicationProcess(id, name, port, logFile); err != nil {
		return err
	}

	return m.SaveState()
}

// StopApplication stops an application by its ID
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
		return fmt.Errorf("application '%s' (ID: %s) is not running", app.Name, id)
	}

	if err := m.stopApplicationProcess(app); err != nil {
		return err
	}

	app.Status = "stopped"
	app.Cmd = nil
	return m.SaveState()
}

// RestartApplication restarts an application by its ID
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

	if err := m.stopApplicationProcess(app); err != nil {
		return fmt.Errorf("failed to stop application: %v", err)
	}

	logFile := filepath.Join(logDir, fmt.Sprintf("%s.log", id))
	if err := m.startApplicationProcess(id, app.Name, app.Port, logFile); err != nil {
		return fmt.Errorf("failed to start application: %v", err)
	}

	return m.SaveState()
}

// StatusApplication returns the status of an application
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

	status := AppStatus{
		ID:     id,
		Name:   app.Name,
		Status: app.Status,
		PID:    app.PID,
		Uptime: m.calculateUptime(app),
	}

	if app.Status == "running" {
		ramUsage, cpuUsage, err := m.getProcessMetrics(app.PID)
		if err != nil {
			return status, fmt.Errorf("failed to get process metrics: %v", err)
		}
		status.RAMUsage = ramUsage
		status.CPUUsage = cpuUsage
	}

	return status, nil
}

// ListApplications returns a list of all applications
func (m *AppManager) ListApplications() []AppListItem {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		fmt.Println("Failed to load state:", err)
		return nil
	}

	var appList []AppListItem
	for id, app := range m.Apps {
		// Only verify binary if the app is running
		if app.Status == "running" && !m.verifyApplicationBinary(id) {
			// If binary is missing for a running app, mark it as stopped
			app.Status = "stopped"
			app.UpdatedAt = time.Now()
			m.SaveState()
		}

		appList = append(appList, AppListItem{
			ID:          id,
			Name:        app.Name,
			Status:      app.Status,
			PID:         app.PID,
			Uptime:      m.calculateUptime(app),
			BuildStatus: app.BuildStatus,
			CreatedAt:   app.CreatedAt,
			UpdatedAt:   app.UpdatedAt,
		})
	}
	return appList
}

// SaveState saves the current state to disk
func (m *AppManager) SaveState() error {
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return err
	}

	type SavedApp struct {
		ID          string    `json:"id"`
		Name        string    `json:"name"`
		PID         int       `json:"pid"`
		Status      string    `json:"status"`
		Start       time.Time `json:"start"`
		Port        int       `json:"port"`
		LogFile     string    `json:"log_file"`
		BuildStatus string    `json:"build_status"`
		CreatedAt   time.Time `json:"created_at"`
		UpdatedAt   time.Time `json:"updated_at"`
	}

	savedApps := make(map[string]SavedApp)
	for id, app := range m.Apps {
		savedApps[id] = SavedApp{
			ID:          id,
			Name:        app.Name,
			PID:         app.PID,
			Status:      app.Status,
			Start:       app.Start,
			Port:        app.Port,
			LogFile:     app.LogFile,
			BuildStatus: app.BuildStatus,
			CreatedAt:   app.CreatedAt,
			UpdatedAt:   app.UpdatedAt,
		}
	}

	data, err := json.MarshalIndent(savedApps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

// LoadState loads the state from disk
func (m *AppManager) LoadState() error {
	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		// If file doesn't exist, create it with empty state
		if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
			return err
		}
		return os.WriteFile(stateFile, []byte("{}"), 0644)
	} else if err != nil {
		return err
	}

	type SavedApp struct {
		ID          string    `json:"id"`
		Name        string    `json:"name"`
		PID         int       `json:"pid"`
		Status      string    `json:"status"`
		Start       time.Time `json:"start"`
		Port        int       `json:"port"`
		LogFile     string    `json:"log_file"`
		BuildStatus string    `json:"build_status"`
		CreatedAt   time.Time `json:"created_at"`
		UpdatedAt   time.Time `json:"updated_at"`
	}

	var savedApps map[string]SavedApp
	if err := json.Unmarshal(data, &savedApps); err != nil {
		return err
	}

	// Find the highest ID to set NextID
	maxID := uint(0)
	for id := range savedApps {
		if idNum, err := strconv.ParseUint(id, 10, 64); err == nil {
			if uint(idNum) > maxID {
				maxID = uint(idNum)
			}
		}
	}
	m.NextID = maxID + 1

	// Clear existing apps and load from saved state
	m.Apps = make(map[string]*AppInfo)
	for id, saved := range savedApps {
		// Check if process is still running
		isRunning := false
		if saved.PID > 0 {
			// Try multiple methods to check if process is running
			if p, err := process.NewProcess(int32(saved.PID)); err == nil {
				if running, _ := p.IsRunning(); running {
					isRunning = true
				}
			}
			// Fallback to system command if gopsutil fails
			if !isRunning {
				cmd := exec.Command("ps", "-p", fmt.Sprintf("%d", saved.PID))
				if err := cmd.Run(); err == nil {
					isRunning = true
				}
			}
		}

		// Create app info with appropriate status
		appInfo := &AppInfo{
			ID:          id,
			Name:        saved.Name,
			PID:         saved.PID,
			Status:      saved.Status,
			Start:       saved.Start,
			Port:        saved.Port,
			LogFile:     saved.LogFile,
			BuildStatus: saved.BuildStatus,
			CreatedAt:   saved.CreatedAt,
			UpdatedAt:   saved.UpdatedAt,
			Cmd:         nil,
		}

		// Update status based on process state
		if isRunning {
			appInfo.Status = "running"
		} else if saved.Status == "running" {
			appInfo.Status = "stopped"
		}

		// Only update timestamp if status changed
		if appInfo.Status != saved.Status {
			appInfo.UpdatedAt = time.Now()
		}

		m.Apps[id] = appInfo
	}

	// Save the updated state
	return m.SaveState()
}

// Helper methods

func (m *AppManager) ensureLogDirectory() error {
	return os.MkdirAll(logDir, 0755)
}

func (m *AppManager) startApplicationProcess(id string, name string, port int, logFile string) error {
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}
	defer f.Close()

	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	cmd.Stdout = f
	cmd.Stderr = f

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start failed: %v", err)
	}

	m.Apps[id] = &AppInfo{
		ID:          id,
		Cmd:         cmd,
		Name:        name,
		PID:         cmd.Process.Pid,
		Status:      "running",
		Start:       time.Now(),
		Port:        port,
		LogFile:     logFile,
		BuildStatus: "built",
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	return nil
}

func (m *AppManager) stopApplicationProcess(app *AppInfo) error {
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
			return fmt.Errorf("failed to stop application '%s' (ID: %s): process still running", app.Name, app.ID)
		}
	}

	return nil
}

func (m *AppManager) calculateUptime(app *AppInfo) string {
	if app.Status != "running" {
		return "N/A"
	}
	duration := time.Since(app.Start)
	return FormatDuration(duration)
}

func (m *AppManager) getProcessMetrics(pid int) (uint64, float64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, 0, err
	}

	var ramUsage uint64
	var cpuUsage float64

	for i := 0; i < 3; i++ {
		time.Sleep(200 * time.Millisecond)
		memInfo, _ := p.MemoryInfo()
		if memInfo != nil {
			ramUsage = memInfo.RSS
		}
		cpuUsage, _ = p.CPUPercent()
		if cpuUsage > 0 {
			break
		}
	}

	return ramUsage, cpuUsage, nil
}

func (m *AppManager) verifyApplicationBinary(id string) bool {
	binaryPath := fmt.Sprintf("./app_%s", id)
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		return false
	}
	return true
}

// FormatDuration formats a duration into a human-readable string
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	return fmt.Sprintf("%02dh %02dm %02ds", h, m, s)
}

// GetGophelApps returns a list of apps that start with "gophel"
func GetGophelApps() ([]string, error) {
	apps := Manager.ListApplications()
	var gophelApps []string

	for _, app := range apps {
		if strings.HasPrefix(app.Name, "gophel") {
			gophelApps = append(gophelApps, app.Name)
		}
	}

	return gophelApps, nil
}
