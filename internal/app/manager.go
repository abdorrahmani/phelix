package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// AppManager manages the lifecycle of applications
type AppManager struct {
	Apps   map[string]*AppInfo
	Lock   sync.Mutex
	NextID uint
}

// Manager is the global instance of AppManager that implements AppManagerInterface
var Manager AppManagerInterface = &AppManager{
	Apps:   make(map[string]*AppInfo),
	NextID: 1,
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
