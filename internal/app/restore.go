package app

import (
	"log"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// RestoreAutoStartApps restarts every application flagged for auto-start that
// is not currently running. It is invoked at the start of the monitor daemon
// (e.g. on machine boot via systemd) so applications that were intentionally
// started before a reboot come back up, while an explicitly-stopped app stays
// down.
//
// It runs inside the monitor process — there is no subprocess exec of
// `phelix start` and no reliance on shell startup ordering. It is safe to call
// when LoadState has already been invoked (it re-loads state under the same
// rule as the rest of the manager).
func (m *AppManager) RestoreAutoStartApps() (int, error) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if err := m.LoadState(); err != nil {
		return 0, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}

	restored := 0
	for id, a := range m.Apps {
		// LoadState has already clamped a dead PID to "stopped"; anything in
		// that state carrying AutoStart intent is a restore candidate.
		if !a.AutoStart || a.Status == "running" {
			continue
		}

		// startApplicationProcess reuses the app's persisted directory, port
		// and log file, and flags the app as auto-start again.
		if err := m.startApplicationProcess(id, a.Name, a.Port, a.LogFile); err != nil {
			log.Printf("[Monitor] Failed to restore app %s (ID: %s): %v", a.Name, id, err)
			continue
		}
		log.Printf("[Monitor] Restored app %s (ID: %s) on port %d", a.Name, id, a.Port)
		restored++
	}

	if restored > 0 {
		// startApplicationProcess updates each AppInfo in memory; persist so a
		// crash right after restore still reflects the running PIDs.
		if err := m.SaveState(); err != nil {
			return restored, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to save state after restoring apps", err)
		}
	}

	return restored, nil
}
