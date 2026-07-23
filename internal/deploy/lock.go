package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DeployLock records an in-flight deploy or rollback in deploy.json so two
// zero-downtime swaps cannot race on the same blue/green slots.
type DeployLock struct {
	Operation string    `json:"operation"`
	StartedAt time.Time `json:"started_at"`
	PID       int       `json:"pid"`
}

// AcquireDeployLock sets op_lock on deploy state when no other operation holds
// it. release must be called when the operation finishes (success or failure).
func AcquireDeployLock(appName, operation string) (release func(), err error) {
	state, err := Load(appName)
	if err != nil {
		if os.IsNotExist(err) {
			state = &DeployState{AppName: appName}
		} else {
			return nil, err
		}
	}
	if state.OpLock != nil {
		return nil, fmt.Errorf("deploy: %q already has %q in progress since %s (pid %d)",
			appName, state.OpLock.Operation, state.OpLock.StartedAt.Format(time.RFC3339), state.OpLock.PID)
	}
	state.OpLock = &DeployLock{
		Operation: operation,
		StartedAt: time.Now(),
		PID:       os.Getpid(),
	}
	if err := Store(state); err != nil {
		return nil, err
	}
	release = func() {
		s, err := Load(appName)
		if err != nil {
			return
		}
		s.OpLock = nil
		_ = Store(s)
	}
	return release, nil
}

// TryLoadLock reads the lock field for tests.
func TryLoadLock(appName string) (*DeployLock, error) {
	s, err := Load(appName)
	if err != nil {
		return nil, err
	}
	return s.OpLock, nil
}

func lockPath(appName string) (string, error) {
	dir, err := appDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "deploy.lock"), nil
}

// MarshalLockFile writes a human-readable lock for debugging (optional).
func MarshalLockFile(appName string, lock DeployLock) error {
	path, err := lockPath(appName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
