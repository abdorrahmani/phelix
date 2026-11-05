package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

// AppManager manages the lifecycle of applications
type AppManager struct {
	Apps   map[string]*AppInfo
	Lock   sync.Mutex
	NextID uint // Deprecated: retained for compatibility with existing state tests.
}

// Manager is the global instance of AppManager that implements AppManagerInterface
var Manager AppManagerInterface = &AppManager{
	Apps:   make(map[string]*AppInfo),
	NextID: 1,
}

// GenerateAppID generates an independent RFC 4122 version 4 application ID.
// The ID is persisted in apps.json and is never derived from an agent/server
// identity or the app name.
func (m *AppManager) GenerateAppID() (string, error) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeUnknown, "failed to genrate application ID", err)
	}

	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// StartApplication starts an application with the given parameters
func (m *AppManager) StartApplication(id string, port int, name string) error {
	return m.StartApplicationWithSecrets(id, port, name, nil)
}

// StartApplicationWithSecrets starts a process with values pinned for this deployment.
func (m *AppManager) StartApplicationWithSecrets(id string, port int, name string, pinned *secrets.PinnedSet) error {
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
	if err := m.startApplicationProcessWithSecrets(id, name, port, logFile, pinned); err != nil {
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
	app.AutoStart = false
	releaseAppSecretCoverage(id)
	return m.SaveState()
}

// releaseAppSecretCoverage drops this process's masked copies of the
// application's secret values; nothing that could emit them is running
// after a full stop.
func releaseAppSecretCoverage(appID string) {
	secrets.ReleaseCoverage(appID)
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

	bindings, err := secrets.CommittedBindings(id)
	if err != nil {
		return err
	}
	var desired, previous *secrets.PinnedSet
	if len(bindings) > 0 || len(app.SecretEnvs) > 0 {
		desired, err = secrets.PinBindings(context.Background(), id, bindings)
		if err != nil {
			return err
		}
		defer desired.Close()
		if m.isProcessRunning(app.PID) {
			names := app.SecretEnvs
			if names == nil {
				for _, b := range bindings {
					names = append(names, b.Env)
				}
			}
			previous, err = secrets.RuntimeForPID(app.PID, names)
			if err != nil {
				return err
			}
			defer previous.Close()
		}
	}
	if err := m.stopApplicationProcess(app); err != nil {
		return err
	}
	logFile := filepath.Join(logDir, fmt.Sprintf("%s.log", id))
	if err := m.startApplicationProcessWithSecrets(id, app.Name, app.Port, logFile, desired); err != nil {
		if previous != nil {
			if restoreErr := m.startApplicationProcessWithSecrets(id, app.Name, app.Port, logFile, previous); restoreErr != nil {
				return errors.Join(err, phelixerr.Wrap(phelixerr.CodeProcessFailed, "restart and restoration both failed", restoreErr), m.SaveState())
			}
		}
		return errors.Join(err, m.SaveState())
	}

	return m.SaveState()
}

// ListApplications returns a list of all applications
func (m *AppManager) ListApplications() []AppListItem {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		logs.Warning("app", "failed to load state: %v", err)
		return nil
	}

	var appList []AppListItem
	for id, app := range m.Apps {
		if app.Status == "running" {
			m.verifyProcessStatus(app)
		}

		appList = append(appList, AppListItem{
			SecretEnvs:  app.SecretEnvs,
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
			Watching:    app.Watching,
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
	status.Watching = app.Watching

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

	if app.Directory != "" {
		binaryPath := filepath.Join(app.Directory, fmt.Sprintf("app_%s", id))
		if err := os.Remove(binaryPath); err != nil && !os.IsNotExist(err) {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to remove application binary %s", binaryPath)
		}
	}

	if app.LogFile != "" {
		if err := os.Remove(app.LogFile); err != nil && !os.IsNotExist(err) {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to remove log file %s", app.LogFile)
		}
	}

	delete(m.Apps, id)

	return m.SaveState()
}
