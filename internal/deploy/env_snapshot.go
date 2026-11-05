package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	envpkg "github.com/abdorrahmani/phelix/internal/env"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/secrets"
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
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: snapshot env v%d", ver)
	}
	return dst, nil
}

// EnvOverlayFromSnapshot decrypts env/vN.enc into KEY=value strings for the
// launcher, then merges the application's committed secret bindings over it.
// Secret values never live in the snapshot: they are read from the secret
// store at launch, so a rollback restores the version's plain variables while
// secrets always resolve to their current values. An application without
// bindings gets exactly the same overlay as before.
func EnvOverlayFromSnapshot(envPath, appID string) ([]string, error) {
	return envOverlay(envPath, appID, nil)
}

// EnvOverlayWithBindings builds the launch overlay the way a deployment does:
// the caller supplies the candidate bindings the deployment pinned, instead
// of reading the committed set from the store. All instances of one
// deployment therefore resolve the identical binding set.
func EnvOverlayWithBindings(envPath, appID string, bindings []secrets.Binding) ([]string, error) {
	return envOverlay(envPath, appID, bindings)
}

// envOverlay is the shared builder: snapshot variables first, secret
// bindings merged over them by name, each name appearing exactly once. An
// empty app identity means the flow carries no application context at all,
// and therefore no bindings either.
func envOverlay(envPath, appID string, bindings []secrets.Binding) ([]string, error) {
	base, err := snapshotOverlay(envPath, appID)
	if err != nil {
		return nil, err
	}
	if appID == "" {
		return base, nil
	}
	if bindings == nil {
		bindings, err = secrets.CommittedBindings(appID)
		if err != nil {
			return nil, err
		}
	}
	if len(bindings) == 0 {
		return base, nil
	}
	overlay, err := secrets.OverlayForLaunch(context.Background(), appID, bindings)
	if err != nil {
		return nil, err
	}
	return mergeByKey(base, overlay), nil
}

// snapshotOverlay decrypts env/vN.enc into KEY=value strings. It is the
// original snapshot-only path, unchanged in behavior.
func snapshotOverlay(envPath, appID string) ([]string, error) {
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
		return nil, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "deploy: parse env snapshot")
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
			// key is the env VAR NAME (e.g. "DATABASE_URL"), never the value —
			// safe to embed.
			return nil, phelixerr.Wrapf(phelixerr.CodeEncryption, err, "deploy: decrypt env %q", key)
		}
		out = append(out, fmt.Sprintf("%s=%s", key, val))
	}
	return out, nil
}

// mergeByKey replaces same-name base entries with overlay entries and keeps
// every other base entry. Managed names therefore appear exactly once, with
// the managed value, no matter what an older snapshot carried.
func mergeByKey(base, overlay []string) []string {
	if len(overlay) == 0 {
		return base
	}
	managed := make(map[string]string, len(overlay))
	for _, kv := range overlay {
		if k, _, ok := splitEnvPair(kv); ok {
			managed[k] = kv
		}
	}
	seen := make(map[string]bool, len(base)+len(overlay))
	out := make([]string, 0, len(base)+len(overlay))
	for _, kv := range base {
		k, _, ok := splitEnvPair(kv)
		if ok {
			if managed[k] != "" || seen[k] {
				continue
			}
			seen[k] = true
		}
		out = append(out, kv)
	}
	for _, kv := range overlay {
		k, _, ok := splitEnvPair(kv)
		if ok {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		out = append(out, kv)
	}
	return out
}

// splitEnvPair splits a KEY=value string, reporting false when the separator
// is missing so a malformed entry passes through rather than being dropped.
func splitEnvPair(kv string) (key, value string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
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
