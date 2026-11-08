package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/resources"
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

	return nil
}

type savedApp struct {
	StartTicks  uint64           `json:"start_ticks,omitempty"`
	SecretEnvs  []string         `json:"secret_envs,omitzero"`
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	PID         int              `json:"pid"`
	Status      string           `json:"status"`
	Start       time.Time        `json:"start"`
	Port        int              `json:"port"`
	LogFile     string           `json:"log_file"`
	BuildStatus string           `json:"build_status"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	Directory   string           `json:"directory"`
	Language    string           `json:"language"`
	AutoStart   bool             `json:"auto_start"`
	Watching    bool             `json:"watching"`
	Resources   resources.Config `json:"resources,omitzero"`
}

// SaveState saves the current state to disk
func (m *AppManager) SaveState() error {

	savedApps := make(map[string]savedApp)
	for id, app := range m.Apps {
		savedApps[id] = savedApp{
			SecretEnvs:  app.SecretEnvs,
			StartTicks:  app.StartTicks,
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
			AutoStart:   app.AutoStart,
			Watching:    app.Watching,
			Resources:   app.Resources,
		}
	}

	data, err := json.Marshal(savedApps)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to marshal application state", err)
	}

	var current map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	fileMutex.Lock()
	defer fileMutex.Unlock()
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return err
	}
	lock, err := lockStateFile(stateFile + ".lock")
	if err != nil {
		return err
	}
	defer unlockStateFile(lock)
	latest := map[string]map[string]json.RawMessage{}
	stored, err := os.ReadFile(stateFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(stored, &latest); err != nil || latest == nil {
			return phelixerr.New(phelixerr.CodeFilesystem, "application state is unreadable; existing state retained")
		}
	}
	for id, fields := range current {
		before := m.savedState[id]
		if before != nil && latest[id] == nil {
			if reflect.DeepEqual(before, fields) {
				continue
			}
			return phelixerr.New(phelixerr.CodeFilesystem, "application was removed during update; reload state before retrying")
		}
		if latest[id] == nil {
			latest[id] = map[string]json.RawMessage{}
		}
		for key, value := range fields {
			if before == nil || !bytes.Equal(value, before[key]) {
				latest[id][key] = value
			}
		}
		for key := range before {
			if _, exists := fields[key]; !exists {
				delete(latest[id], key)
			}
		}
	}
	for id := range m.savedState {
		if _, exists := current[id]; !exists {
			delete(latest, id)
		}
	}
	data, err = json.MarshalIndent(latest, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAppState(data); err != nil {
		return err
	}
	m.savedState = current
	return nil
}

// writeAppState replaces the state only after its complete contents reach disk.
func writeAppState(data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(stateFile), ".apps-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0644); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), stateFile); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(stateFile))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// LoadState reads apps.json into the in-memory registry, clamping dead PIDs
// to stopped. It takes the manager lock: concurrent callers (the monitor's
// metrics tick, remote commands, lifecycle paths) serialize on the lock
// instead of racing on the shared app map.
func (m *AppManager) LoadState() error {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	return m.loadStateLocked()
}

// loadStateLocked is LoadState's body for callers that already hold m.Lock.
// Calling LoadState while holding the lock would deadlock (a sync.Mutex is
// not reentrant), so locked readers use this variant directly.
func (m *AppManager) loadStateLocked() error {
	fileMutex.Lock()
	data, err := os.ReadFile(stateFile)
	fileMutex.Unlock()
	if os.IsNotExist(err) {
		data = []byte("{}")
	} else if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read application state", err)
	}

	var savedApps map[string]savedApp
	if err := json.Unmarshal(data, &savedApps); err != nil {
		return phelixerr.New(phelixerr.CodeFilesystem, "application state is unreadable; existing state retained")
	}
	if savedApps == nil {
		return phelixerr.New(phelixerr.CodeFilesystem, "application state is invalid; existing state retained")
	}

	baseline, err := json.Marshal(savedApps)
	if err != nil {
		return err
	}
	var savedState map[string]map[string]json.RawMessage
	if err := json.Unmarshal(baseline, &savedState); err != nil {
		return err
	}
	m.savedState = savedState

	// Clear existing apps and load from saved state
	previousApps := m.Apps
	m.Apps = make(map[string]*AppInfo)
	for id, saved := range savedApps {
		// Check if process is still running
		isRunning := m.appProcessRunning(&AppInfo{PID: saved.PID, StartTicks: saved.StartTicks})

		// Create app info with appropriate status
		appInfo := &AppInfo{
			SecretEnvs:  saved.SecretEnvs,
			StartTicks:  saved.StartTicks,
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
		if previous := previousApps[id]; previous != nil {
			if isRunning && previous.PID == saved.PID {
				appInfo.Cmd = previous.Cmd
				appInfo.resourceInstance = previous.resourceInstance
				appInfo.logFileHandle = previous.logFileHandle
				appInfo.releaseSecrets = previous.releaseSecrets
			} else if !m.appProcessRunning(previous) {
				m.waitAndCloseLog(previous)
			}
		}
		// restore language and no-upload flag
		appInfo.Language = saved.Language

		// Watching is a plain copy: state files that predate the field
		// unmarshal to false, which is the required default — an upgrade
		// must never opt an existing app into backend monitoring.
		appInfo.Watching = saved.Watching
		appInfo.Resources = saved.Resources

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

	return nil
}

// AppDirectories returns a name-to-project-directory snapshot parsed
// straight from apps.json. Unlike LoadState it never mutates the shared
// manager, so a concurrent reader such as the worker supervisor tick cannot
// race the monitor's other goroutines on the Apps map.
func AppDirectories() map[string]string {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil
	}
	var savedApps map[string]struct {
		Name      string `json:"name"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(data, &savedApps); err != nil {
		return nil
	}
	out := make(map[string]string, len(savedApps))
	for _, saved := range savedApps {
		if saved.Name != "" {
			out[saved.Name] = saved.Directory
		}
	}
	return out
}

// SetStateDirForTesting points the package state file at dir/apps.json
// and reloads it. It exists for tests in other packages that exercise the
// in-process application registry without a subprocess; production code
// must never call it.
func SetStateDirForTesting(dir string) error {
	stateFile = filepath.Join(dir, "apps.json")
	return Manager.LoadState()
}
