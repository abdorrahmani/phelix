package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

var (
	stateFile string
	logDir    string
	fileMutex sync.Mutex
)

// init initializes the package by setting up state file and log directory paths
// and ensuring required directories exist.
func init() {
	dataDir := runtimeDataDir()
	stateFile = filepath.Join(dataDir, "apps.json")
	logDir = filepath.Join(dataDir, "logs")

	// Ensure directories exist. This runs at package-init time, before any
	// error boundary exists, so the diagnostic goes to the daemon log (stderr)
	// rather than polluting stdout — and it cannot be returned to the caller.
	if err := ensureDirectories(); err != nil {
		logs.Warning("app", "failed to create required directories: %v", err)
	}
}

// runtimeDataDir matches the agent identity directory without importing the
// server package (which would create an import cycle). Docker uses the mounted
// /var/lib/phelix volume; local installations retain ~/.phelix by default.
func runtimeDataDir() string {
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

// ensureDirectories creates the necessary directories and initializes the state file if it doesn't exist.
func ensureDirectories() error {
	// Create .phelix directory
	phelixDir := filepath.Dir(stateFile)
	if err := os.MkdirAll(phelixDir, 0755); err != nil {
		return fmt.Errorf("failed to create .phelix directory: %w", err)
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
		Language    string    `json:"language"`
		NoUpload    bool      `json:"no_upload"`
		AutoStart   bool      `json:"auto_start"`
		Watching    bool      `json:"watching"`
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
			Language:    app.Language,
			NoUpload:    app.NoUpload,
			AutoStart:   app.AutoStart,
			Watching:    app.Watching,
		}
	}

	data, err := json.MarshalIndent(savedApps, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to marshal application state", err)
	}

	// Serialize file operations to avoid concurrent writes
	fileMutex.Lock()
	defer fileMutex.Unlock()

	tmpFile := stateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write state file %s", tmpFile)
	}
	if err := os.Rename(tmpFile, stateFile); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to replace state file %s", stateFile)
	}
	return nil
}

// LoadState loads the state from disk
func (m *AppManager) LoadState() error {
	// Ensure only one goroutine performs file read/repair at a time
	fileMutex.Lock()
	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		// If file doesn't exist, create it with empty state
		if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
			fileMutex.Unlock()
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to create state directory", err)
		}
		if err := os.WriteFile(stateFile, []byte("{}"), 0644); err != nil {
			fileMutex.Unlock()
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to initialize state file", err)
		}
		// Release lock and return after creating file
		fileMutex.Unlock()
		return nil
	} else if err != nil {
		fileMutex.Unlock()
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read state file %s", stateFile)
	}
	fileMutex.Unlock()

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
		Language    string    `json:"language"`
		NoUpload    bool      `json:"no_upload"`
		AutoStart   bool      `json:"auto_start"`
		Watching    bool      `json:"watching"`
	}

	var savedApps map[string]SavedApp
	if err := json.Unmarshal(data, &savedApps); err != nil {
		// Handle corrupted JSON: back up corrupted file and reinitialize
		corruptName := stateFile + ".corrupt." + fmt.Sprintf("%d", time.Now().Unix())
		_ = os.WriteFile(corruptName, data, 0644)

		// Recreate a fresh state file
		fileMutex.Lock()
		_ = os.WriteFile(stateFile, []byte("{}"), 0644)
		fileMutex.Unlock()

		// Continue with empty state instead of failing
		savedApps = make(map[string]SavedApp)
	}

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
		// restore language and no-upload flag
		appInfo.Language = saved.Language
		appInfo.NoUpload = saved.NoUpload

		// Watching is a plain copy: state files that predate the field
		// unmarshal to false, which is the required default — an upgrade
		// must never opt an existing app into backend monitoring.
		appInfo.Watching = saved.Watching

		// AutoStart records that this app was intentionally started; on a fresh
		// daemon launch (boot / monitor restart) it is the signal that the app
		// should be restored. Seed it from legacy state files: anything saved
		// as "running" gets auto-start intent even if the "auto_start" field
		// predates this version.
		appInfo.AutoStart = saved.AutoStart || saved.Status == "running"

		// Update status based on process state
		if isRunning {
			appInfo.Status = "running"
		} else if saved.Status == "running" {
			appInfo.Status = "stopped"
			// Do not retain a dead PID. Apart from being misleading in status
			// output, a zombie PID may remain visible in /proc indefinitely when
			// its parent does not reap it.
			appInfo.PID = 0
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
