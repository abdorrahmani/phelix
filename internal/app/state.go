package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

var (
	stateFile string
	logDir    string
	fileMutex sync.Mutex
)

// init initializes the package by setting up state file and log directory paths
// and ensuring required directories exist.
func init() {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE") // For Windows
	}
	stateFile = filepath.Join(homeDir, ".phelix", "apps.json")
	logDir = filepath.Join(homeDir, ".phelix", "logs")

	// Ensure directories exist
	if err := ensureDirectories(); err != nil {
		fmt.Printf("Warning: Failed to create required directories: %v\n", err)
	}
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
		}
	}

	data, err := json.MarshalIndent(savedApps, "", "  ")
	if err != nil {
		return err
	}

	// Serialize file operations to avoid concurrent writes
	fileMutex.Lock()
	defer fileMutex.Unlock()

	tmpFile := stateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, stateFile)
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
			return err
		}
		if err := os.WriteFile(stateFile, []byte("{}"), 0644); err != nil {
			fileMutex.Unlock()
			return err
		}
		// Release lock and return after creating file
		fileMutex.Unlock()
		return nil
	} else if err != nil {
		fileMutex.Unlock()
		return err
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
		// restore language and no-upload flag
		appInfo.Language = saved.Language
		appInfo.NoUpload = saved.NoUpload

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
