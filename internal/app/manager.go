package app

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}

	if app, exists := m.Apps[id]; exists {
		if app.Status == "running" {
			return phelixerr.Newf(
				phelixerr.CodeAlreadyExists,
				"application '%s' (ID: %s) is already running on PID %d",
				app.Name, id, app.PID,
			)
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
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}

	app, exists := m.Apps[id]
	if !exists {
		return phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found", id)
	}

	if app.Status != "running" {
		// State can be stale when an earlier Phelix invocation was terminated or
		// the child was reaped by another parent. Stopping an already-exited app
		// is successful and makes `phelix stop` safe to retry.
		if !m.isProcessRunning(app.PID) {
			app.Status = "stopped"
			app.PID = 0
			app.Cmd = nil
			app.AutoStart = false
			app.UpdatedAt = time.Now()
			return m.SaveState()
		}
		return phelixerr.Newf(phelixerr.CodeProcessFailed, "application '%s' (ID: %s) is not running", app.Name, id)
	}

	if err := m.stopApplicationProcess(app); err != nil {
		return err
	}

	app.Status = "stopped"
	app.PID = 0
	app.Cmd = nil
	// An explicit stop clears auto-start intent: the app must not be restored
	// automatically the next time the monitor daemon launches.
	app.AutoStart = false
	return m.SaveState()
}

// RestartApplication restarts an application by its ID
func (m *AppManager) RestartApplication(id string) error {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}

	app, exists := m.Apps[id]
	if !exists {
		return phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found", id)
	}

	if err := m.stopApplicationProcess(app); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to stop application", err)
	}

	logFile := filepath.Join(logDir, fmt.Sprintf("%s.log", id))
	if err := m.startApplicationProcess(id, app.Name, app.Port, logFile); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to start application", err)
	}

	return m.SaveState()
}

// ListApplications returns a list of all applications
func (m *AppManager) ListApplications() []AppListItem {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		// ListApplications cannot fail the caller (its signature returns only a
		// list), but the diagnostic must not pollute stdout — a caller's
		// processable output. Emit it on the daemon log instead; callers that
		// need strict behavior check LoadState themselves.
		log.Printf("[List] Failed to load state: %v", err)
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
			Directory:   app.Directory,
			Status:      app.Status,
			PID:         app.PID,
			Port:        app.Port,
			Uptime:      m.calculateUptime(app),
			BuildStatus: app.BuildStatus,
			CreatedAt:   app.CreatedAt,
			UpdatedAt:   app.UpdatedAt,
			Language:    app.Language,
			Process:     app.Config().Process,
			Networking:  app.Config().Networking,
			Logging:     app.Config().Logging,
			Storage:     app.Config().Storage,
		})
	}
	return appList
}

// StatusApplication returns the status of an application by its ID or AppName
func (m *AppManager) StatusApplication(identifier string) (AppStatus, error) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return AppStatus{}, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}

	app, exists := m.Apps[identifier]
	if !exists {
		for id, a := range m.Apps {
			if a.Name == identifier {
				app = a
				identifier = id
				exists = true
				break
			}
		}
	}
	if !exists {
		return AppStatus{}, phelixerr.Newf(phelixerr.CodeNotFound, "application not found with ID or Name: %s", identifier)
	}

	// Verify process status
	if app.Status == "running" {
		m.verifyProcessStatus(app)
	}

	status := AppStatus{
		ID:          identifier,
		Name:        app.Name,
		Status:      app.Status,
		PID:         app.PID,
		Uptime:      m.calculateUptime(app),
		BuildStatus: app.BuildStatus,
		CreatedAt:   app.CreatedAt,
		UpdatedAt:   app.UpdatedAt,
	}
	status.Language = app.Language

	if app.Status == "running" {
		ramUsage, cpuUsage, err := m.getProcessMetrics(app.PID)
		if err != nil {
			return status, phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to get process metrics", err)
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
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}

	app, exists := m.Apps[id]
	if !exists {
		return phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found", id)
	}

	// Stop the application if it's running
	if app.Status == "running" {
		if err := m.stopApplicationProcess(app); err != nil {
			return phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to stop application before removal", err)
		}
	}

	// Remove the application binary if it exists
	if app.Directory != "" {
		binaryPath := filepath.Join(app.Directory, fmt.Sprintf("app_%s", id))
		if err := os.Remove(binaryPath); err != nil && !os.IsNotExist(err) {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to remove application binary %s", binaryPath)
		}
	}

	// Remove the log file if it exists
	if app.LogFile != "" {
		if err := os.Remove(app.LogFile); err != nil && !os.IsNotExist(err) {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to remove log file %s", app.LogFile)
		}
	}

	// Remove the application from the map
	delete(m.Apps, id)

	// Save the updated state
	return m.SaveState()
}
