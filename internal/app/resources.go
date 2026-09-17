package app

import (
	"encoding/json"
	"os"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/resources"
)

// LoadResources returns the persisted per-instance resource policy for the
// named app, read straight from apps.json without touching the global
// manager: it must never rewrite state (LoadState would normalize statuses
// and re-save), mutate in-memory records, or race a concurrent writer beyond
// the shared fileMutex window.
//
// Decode is partial — only each record's "name" and "resources" keys are
// unmarshalled — so unknown or unrelated fields stay untouched and legacy
// state files (no resources key) decode as the zero config, i.e. unlimited.
//
// Missing or unreadable runtime state fails closed; it cannot establish that
// an app has no limits. Only an existing record without resources is unlimited.
func LoadResources(appName string) (resources.Config, error) {
	fileMutex.Lock()
	data, err := os.ReadFile(stateFile)
	fileMutex.Unlock()

	if err != nil {
		return resources.Config{}, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to read state file %s", stateFile)
	}

	var savedApps map[string]struct {
		Name      string           `json:"name"`
		Resources resources.Config `json:"resources"`
	}
	if err := json.Unmarshal(data, &savedApps); err != nil {
		return resources.Config{}, phelixerr.Wrapf(phelixerr.CodeConfiguration, err,
			"state file %s is malformed; inspect or restore it before starting the app", stateFile)
	}

	for _, saved := range savedApps {
		if saved.Name == appName {
			if err := saved.Resources.Validate(); err != nil {
				return resources.Config{}, phelixerr.Wrap(phelixerr.CodeConfiguration, "invalid persisted resource limits", err)
			}
			return saved.Resources, nil
		}
	}
	return resources.Config{}, phelixerr.Newf(phelixerr.CodeConfiguration, "cannot load resource policy: application %q missing from %s; restore runtime state before starting", appName, stateFile)
}
