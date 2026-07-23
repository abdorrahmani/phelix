package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	envpkg "github.com/abdorrahmani/phelix/internal/env"
)

// snapshotEnvForVersion copies the app's live encrypted env store to env/vN.enc.
func snapshotEnvForVersion(appName, appID string, ver int) (string, error) {
	src, err := envpkg.GetEnvFilePath(appID)
	if err != nil {
		return "", err
	}
	dst, err := envSnapshotPath(appName, ver)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(src); os.IsNotExist(err) {
		// No env configured — create an empty store snapshot for pairing consistency.
		empty := envpkg.EnvStore{Version: 1, AppID: appID, Entries: map[string]envpkg.EnvEntry{}}
		data, err := json.MarshalIndent(empty, "", "  ")
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", err
		}
		return dst, nil
	}
	if err := copyFile(src, dst, 0o644); err != nil {
		return "", fmt.Errorf("deploy: snapshot env v%d: %w", ver, err)
	}
	return dst, nil
}

// EnvOverlayFromSnapshot decrypts env/vN.enc into KEY=value strings for the launcher.
func EnvOverlayFromSnapshot(envPath, appID string) ([]string, error) {
	if envPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var store envpkg.EnvStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("deploy: parse env snapshot: %w", err)
	}
	if store.AppID == "" {
		store.AppID = appID
	}
	// Use the manager's decryption path via a temporary load pattern.
	masterKey, err := envpkg.GetMasterKey()
	if err != nil {
		return nil, err
	}
	var out []string
	for key, entry := range store.Entries {
		val, err := envpkg.DecryptData(entry.Value, masterKey)
		if err != nil {
			return nil, fmt.Errorf("deploy: decrypt env %q: %w", key, err)
		}
		out = append(out, fmt.Sprintf("%s=%s", key, val))
	}
	return out, nil
}

// RollbackEvent is logged and optionally sent via Notifier after rollback attempts.
type RollbackEvent struct {
	AppName   string
	FromVer   int
	ToVer     int
	Timestamp time.Time
	Success   bool
	ErrMsg    string
}

func FormatRollbackEvent(ev RollbackEvent) string {
	status := "succeeded"
	if !ev.Success {
		status = "failed: " + ev.ErrMsg
	}
	return fmt.Sprintf("rollback %s: v%d → v%d at %s — %s",
		ev.AppName, ev.FromVer, ev.ToVer, ev.Timestamp.Format(time.RFC3339), status)
}
