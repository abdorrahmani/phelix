package app

import (
	"errors"
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

var RestoreAppGuard func(id, name string) (func(), error)

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
	if err := m.LoadState(); err != nil {
		return 0, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}
	m.Lock.Lock()
	var candidates []AppInfo
	for _, application := range m.Apps {
		if application.AutoStart && application.Status != "running" {
			candidates = append(candidates, *application)
		}
	}
	m.Lock.Unlock()
	restored := 0
	for _, application := range candidates {
		id := application.ID
		if DeployedAppSkipper != nil && DeployedAppSkipper(id) {
			logs.Info("app", "skipping classic restore for %s (ID: %s): managed by zero-downtime deployment", application.Name, id)
			continue
		}
		release := func() {}
		if RestoreAppGuard != nil {
			var err error
			release, err = RestoreAppGuard(id, application.Name)
			if err != nil {
				logs.Warning("app", "cannot restore app %s (ID: %s): %v", application.Name, id, err)
				continue
			}
		}
		started, err := m.restoreClassicApplication(id)
		release()
		if err != nil {
			logs.Error("app", "failed to restore app %s (ID: %s): %v", application.Name, id, err)
			continue
		}
		if started {
			logs.Info("app", "restored app %s (ID: %s) on port %d", application.Name, id, application.Port)
			restored++
		}
	}
	return restored, nil
}

func (m *AppManager) restoreClassicApplication(id string) (bool, error) {
	m.Lock.Lock()
	defer m.Lock.Unlock()
	if err := m.loadStateLocked(); err != nil {
		return false, err
	}
	application := m.Apps[id]
	if application == nil || !application.AutoStart || application.Status == "running" {
		return false, nil
	}
	logFile := application.LogFile
	if logFile == "" {
		logFile = logs.AppLogPath(id)
	}
	if err := m.startApplicationProcess(id, application.Name, application.Port, logFile); err != nil {
		return false, err
	}
	if err := m.SaveState(); err != nil {
		return false, errors.Join(err, m.stopNewlyStarted(application))
	}
	return true, nil
}
