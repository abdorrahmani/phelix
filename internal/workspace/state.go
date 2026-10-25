package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// StateFileName is the persisted orchestration record of one workspace,
// stored under <dataDir>/workspaces/<name>/.
const StateFileName = "workspace.json"

// LockFileName is the workspace-level lock guarding orchestration.
const LockFileName = "workspace.lock"

// ServiceState records the last workspace-level outcome for one service.
// Runtime truth still lives in the per-app stores; this file records
// membership and orchestration history so a workspace command can report and
// scripts can inspect what the last build or deploy did to the mesh.
type ServiceState struct {
	Name          string    `json:"name"`
	Path          string    `json:"path"`
	Port          int       `json:"port,omitempty"`
	Strategy      string    `json:"strategy,omitempty"`
	DependsOn     []string  `json:"depends_on,omitempty"`
	LastOperation string    `json:"last_operation,omitempty"`
	LastResult    string    `json:"last_result,omitempty"`
	LastVersion   int       `json:"last_version,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// State is the persisted record of one workspace: its root, definition
// source, member snapshot, and the last orchestration outcome per service.
type State struct {
	Name      string         `json:"name"`
	Root      string         `json:"root"`
	Source    string         `json:"source"`
	Services  []ServiceState `json:"services"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// StateDir returns the directory all workspace state lives under,
// following the same data-dir resolution as the rest of Phelix
// (PHELIX_DATA_DIR, then /var/lib/phelix inside Docker, then ~/.phelix).
func StateDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}
	base := filepath.Join(home, ".phelix")
	if dir := os.Getenv("PHELIX_DATA_DIR"); dir != "" {
		base = dir
	} else if _, err := os.Stat("/.dockerenv"); err == nil {
		base = "/var/lib/phelix"
	}
	return filepath.Join(base, "workspaces"), nil
}

func statePath(name string) (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name, StateFileName), nil
}

// LoadState reads the persisted state of a workspace. A workspace that has
// never been orchestrated has no state file; that is (nil, nil), not an
// error, so read-only commands do not fabricate records.
func LoadState(name string) (*State, error) {
	path, err := statePath(name)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read %s", path)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "%s is corrupt", path)
	}
	return &state, nil
}

// SaveState atomically persists the workspace state file.
func SaveState(state *State) error {
	path, err := statePath(state.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create workspace state dir")
	}
	state.UpdatedAt = time.Now()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeConfiguration, "failed to encode workspace state", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to write %s", tmp)
	}
	if err := os.Rename(tmp, path); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to replace %s", path)
	}
	return nil
}

// SyncMembership rewrites the state's service snapshot from a resolved
// workspace, preserving the last orchestration outcome of services that are
// still members and dropping the ones that left.
func (s *State) SyncMembership(w *Workspace) {
	previous := make(map[string]ServiceState, len(s.Services))
	for _, svc := range s.Services {
		previous[svc.Name] = svc
	}
	s.Name = w.Name
	s.Root = w.Root
	s.Source = w.Source
	s.Services = make([]ServiceState, 0, len(w.Services))
	for _, svc := range w.Services {
		next := ServiceState{
			Name:          svc.Name,
			Path:          svc.Path,
			Port:          svc.Port,
			Strategy:      svc.Strategy,
			DependsOn:     append([]string(nil), svc.DependsOn...),
			LastOperation: previous[svc.Name].LastOperation,
			LastResult:    previous[svc.Name].LastResult,
			LastVersion:   previous[svc.Name].LastVersion,
			LastError:     previous[svc.Name].LastError,
			UpdatedAt:     previous[svc.Name].UpdatedAt,
		}
		s.Services = append(s.Services, next)
	}
}

// RecordServiceResult stores the outcome of one orchestrated service
// operation on the in-memory state. Persist with SaveState.
func (s *State) RecordServiceResult(name, operation, result string, version int, lastErr string) {
	for i := range s.Services {
		if s.Services[i].Name != name {
			continue
		}
		s.Services[i].LastOperation = operation
		s.Services[i].LastResult = result
		s.Services[i].LastVersion = version
		s.Services[i].LastError = lastErr
		s.Services[i].UpdatedAt = time.Now()
		return
	}
}
