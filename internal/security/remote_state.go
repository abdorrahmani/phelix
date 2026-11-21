package security

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type RemoteState struct {
	Enabled        bool      `json:"enabled"`
	UpdatedAt      time.Time `json:"updated_at"`
	Revision       uint64    `json:"revision"`
	SyncedRevision uint64    `json:"synced_revision"`
	SyncStatus     string    `json:"sync_status"`
	SyncError      string    `json:"sync_error,omitempty"`
}

func remoteStatePath(dataDir string) string {
	return filepath.Join(dataDir, "security", "remote.json")
}

func LoadRemoteState(dataDir string) (RemoteState, error) {
	state := RemoteState{SyncStatus: "pending"}
	raw, err := os.ReadFile(remoteStatePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(raw) > 4096 {
		return state, errors.New("remote security state is too large")
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return RemoteState{}, errors.New("remote security state is malformed")
	}
	if state.SyncStatus != "pending" && state.SyncStatus != "synchronized" && state.SyncStatus != "failed" {
		return RemoteState{}, errors.New("remote security synchronization state is invalid")
	}
	return state, nil
}

func updateRemoteState(dataDir string, change func(*RemoteState) error) (RemoteState, error) {
	path := remoteStatePath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return RemoteState{}, err
	}
	lock, err := acquireLock(path+".lock", true)
	if err != nil {
		return RemoteState{}, err
	}
	defer lock.Close()
	state, err := LoadRemoteState(dataDir)
	if err != nil {
		return state, err
	}
	if err := change(&state); err != nil {
		return state, err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return state, err
	}
	return state, writeFileAtomic(path, raw, 0600)
}

func SetRemoteSecurity(dataDir string, enabled bool) (RemoteState, error) {
	return updateRemoteState(dataDir, func(state *RemoteState) error {
		if state.Revision > 0 && state.Enabled == enabled {
			return nil
		}
		state.Enabled = enabled
		state.Revision++
		state.UpdatedAt = time.Now().UTC()
		state.SyncStatus = "pending"
		state.SyncError = ""
		return nil
	})
}

func AcknowledgeRemoteSecurity(dataDir string, revision uint64, syncError string) error {
	_, err := updateRemoteState(dataDir, func(state *RemoteState) error {
		if state.Revision != revision {
			return nil
		}
		if len(syncError) > 512 {
			syncError = syncError[:512]
		}
		state.SyncError = syncError
		state.SyncStatus = "failed"
		if syncError == "" {
			state.SyncedRevision = revision
			state.SyncStatus = "synchronized"
		}
		return nil
	})
	return err
}

func RemoteSecurityEnabled(dataDir string) bool {
	state, err := LoadRemoteState(dataDir)
	return err == nil && state.Enabled
}
