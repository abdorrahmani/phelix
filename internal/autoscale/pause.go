package autoscale

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// PauseState is the runtime pause/resume intent for one app's autoscaler,
// persisted next to deploy.json so it survives monitor restarts. It
// composes with the phelix.yaml enabled flag: the effective controller gate
// is enabled AND NOT paused. Pause is operator intent, so unlike cooldown
// and evaluation windows it is durable.
type PauseState struct {
	Paused   bool      `json:"paused"`
	PausedAt time.Time `json:"paused_at,omitempty"`
	PausedBy string    `json:"paused_by,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// pausePath returns the app's autoscale runtime-state file, following the
// same per-app directory convention as deploy.json.
func pausePath(app string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".phelix", "apps", app, "autoscale.json"), nil
}

// LoadPauseState returns the app's pause state. A missing file is the zero
// state (not paused), not an error.
func LoadPauseState(app string) (PauseState, error) {
	path, err := pausePath(app)
	if err != nil {
		return PauseState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return PauseState{}, nil
		}
		return PauseState{}, err
	}
	var st PauseState
	if err := json.Unmarshal(data, &st); err != nil {
		// An unreadable pause file must never wedge the controller into
		// permanently paused: treat it as not paused.
		return PauseState{}, nil
	}
	return st, nil
}

// SavePauseState writes the pause state with the atomic tmp+rename
// discipline so a crash never leaves a truncated intent file.
func SavePauseState(app string, st PauseState) error {
	path, err := pausePath(app)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".autoscale-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Pause marks the app's autoscaler paused by the named operator.
func Pause(app, by, reason string) error {
	return SavePauseState(app, PauseState{Paused: true, PausedAt: time.Now().UTC(), PausedBy: by, Reason: reason})
}

// Resume clears the app's pause.
func Resume(app string) error {
	return SavePauseState(app, PauseState{})
}
