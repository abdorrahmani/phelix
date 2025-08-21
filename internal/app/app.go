package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	RemoveApplication(id string) error
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
	Directory   string // Directory where the application is located
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
	Port        int
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

var (
	stateFile string
	logDir    string
)

func init() {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE") // For Windows
	}
	stateFile = filepath.Join(homeDir, ".gophel", "apps.json")
	logDir = filepath.Join(homeDir, ".gophel", "logs")

	// Ensure directories exist
	if err := ensureDirectories(); err != nil {
		fmt.Printf("Warning: Failed to create required directories: %v\n", err)
	}
}

func ensureDirectories() error {
	// Create .gophel directory
	gophelDir := filepath.Dir(stateFile)
	if err := os.MkdirAll(gophelDir, 0755); err != nil {
		return fmt.Errorf("failed to create .gophel directory: %w", err)
	}

	// Create logs directory
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to create logs directory: %w", err)
	}

	// Initialize state file if it doesn't exist
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		if err := os.WriteFile(stateFile, []byte("{}"), 0644); err != nil {
			return fmt.Errorf("failed to initialize state file: %w", err)
		}
	}

	return nil
}

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

// verifyProcessStatus checks if a process is running and updates its status
func (m *AppManager) verifyProcessStatus(app *AppInfo) bool {
	if app.Status != "running" || app.PID <= 0 {
		return false
	}

	// Try using gopsutil first
	if p, err := process.NewProcess(int32(app.PID)); err == nil {
		if running, _ := p.IsRunning(); running {
			// Additional verification using system commands
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", app.PID))
			} else {
				cmd = exec.Command("ps", "-p", fmt.Sprintf("%d", app.PID))
			}
			if err := cmd.Run(); err == nil {
				return true
			}
		}
	}

	// If we get here, the process is not running
	app.Status = "stopped"
	app.UpdatedAt = time.Now()
	m.SaveState()
	return false
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
		// Verify process status for running applications
		if app.Status == "running" {
			m.verifyProcessStatus(app)
		}

		appList = append(appList, AppListItem{
			ID:          id,
			Name:        app.Name,
			Status:      app.Status,
			PID:         app.PID,
			Port:        app.Port,
			Uptime:      m.calculateUptime(app),
			BuildStatus: app.BuildStatus,
			CreatedAt:   app.CreatedAt,
			UpdatedAt:   app.UpdatedAt,
		})
	}
	return appList
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

	// Verify process status
	if app.Status == "running" {
		m.verifyProcessStatus(app)
	}

	status := AppStatus{
		ID:          id,
		Name:        app.Name,
		Status:      app.Status,
		PID:         app.PID,
		Uptime:      m.calculateUptime(app),
		BuildStatus: app.BuildStatus,
		CreatedAt:   app.CreatedAt,
		UpdatedAt:   app.UpdatedAt,
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

// SaveState saves the current state to disk
func (m *AppManager) SaveState() error {
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
		Directory   string    `json:"directory"`
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
			Directory:   app.Directory,
		}
	}

	data, err := json.MarshalIndent(savedApps, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := stateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, stateFile)
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
		Directory   string    `json:"directory"`
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
		isRunning := m.isProcessRunning(saved.PID)

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
			Directory:   saved.Directory,
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

	app, exists := m.Apps[id]
	if !exists {
		return fmt.Errorf("application %s not found", id)
	}

	if app.Directory == "" {
		return fmt.Errorf("application directory not found for ID %s", id)
	}

	binaryPath := filepath.Join(app.Directory, fmt.Sprintf("app_%s", id))
	cmd := exec.Command(binaryPath)
	cmd.Dir = app.Directory
	cmd.Stdout = f
	cmd.Stderr = f

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start failed: %v", err)
	}

	app.Cmd = cmd
	app.PID = cmd.Process.Pid
	app.Status = "running"
	app.Start = time.Now()
	app.Port = port
	app.LogFile = logFile
	app.BuildStatus = "built"
	app.UpdatedAt = time.Now()

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

	// Check if process is actually running
	running, err := p.IsRunning()
	if err != nil || !running {
		return 0, 0, fmt.Errorf("process is not running")
	}

	var ramUsage uint64
	var cpuUsage float64

	// Try to get memory info
	memInfo, err := p.MemoryInfo()
	if err == nil && memInfo != nil {
		ramUsage = memInfo.RSS
	}

	// Try to get CPU usage
	cpuPercent, err := p.CPUPercent()
	if err == nil {
		cpuUsage = cpuPercent
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

// isProcessRunning checks if a process is running using multiple methods
func (m *AppManager) isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}

	// Try using gopsutil first
	if p, err := process.NewProcess(int32(pid)); err == nil {
		if running, _ := p.IsRunning(); running {
			// Additional verification using system commands
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid))
			} else {
				cmd = exec.Command("ps", "-p", fmt.Sprintf("%d", pid))
			}
			if err := cmd.Run(); err == nil {
				return true
			}
		}
	}

	return false
}

// RemoveApplication removes an application by its ID
func (m *AppManager) RemoveApplication(id string) error {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return fmt.Errorf("failed to load state: %v", err)
	}

	app, exists := m.Apps[id]
	if !exists {
		return fmt.Errorf("application %s not found", id)
	}

	// Stop the application if it's running
	if app.Status == "running" {
		if err := m.stopApplicationProcess(app); err != nil {
			return fmt.Errorf("failed to stop application before removal: %v", err)
		}
	}

	// Remove the application binary if it exists
	if app.Directory != "" {
		binaryPath := filepath.Join(app.Directory, fmt.Sprintf("app_%s", id))
		if err := os.Remove(binaryPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove application binary: %v", err)
		}
	}

	// Remove the log file if it exists
	if app.LogFile != "" {
		if err := os.Remove(app.LogFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove log file: %v", err)
		}
	}

	// Remove the application from the map
	delete(m.Apps, id)

	// Save the updated state
	return m.SaveState()
}
