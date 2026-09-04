package app

import (
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// DeployedAppSkipper, when set, is consulted before the classic restore of
// each auto-start app. It returns true for apps managed by a zero-downtime
// deployment strategy; those must NOT be classic-started on the public port
// (that would fight the proxy for port ownership). The cmd layer wires this
// hook — app cannot import the deploy package (deploy → health → app would
// cycle), and the deploy layer restores its own apps.
var DeployedAppSkipper func(id string) bool

// DeployedAppRestarter, when set, restarts an app managed by a zero-downtime
// deployment the way `phelix restart` does: tear the deployment down, then
// relaunch its instances and re-establish the proxy route. Callers that would
// otherwise reach for RestartApplication must prefer it for any app
// DeployedAppSkipper claims — the single-PID restart would kill the serving
// instance and then try to bind the proxy-owned public port, leaving the app
// down. Wired by the cmd layer for the same import-cycle reason.
var DeployedAppRestarter func(id string) error

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
		if DeployedAppSkipper != nil && DeployedAppSkipper(id) {
			logs.Info("app", "skipping classic restore for %s (ID: %s): managed by zero-downtime deployment", a.Name, id)
			continue
		}

		// startApplicationProcess reuses the app's persisted directory, port
		// and log file, and flags the app as auto-start again. Legacy state
		// files may carry an empty log_file; fall back to the canonical path.
		logFile := a.LogFile
		if logFile == "" {
			logFile = logs.AppLogPath(id)
		}
		if err := m.startApplicationProcess(id, a.Name, a.Port, logFile); err != nil {
			logs.Error("app", "failed to restore app %s (ID: %s): %v", a.Name, id, err)
			continue
		}
		logs.Info("app", "restored app %s (ID: %s) on port %d", a.Name, id, a.Port)
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
