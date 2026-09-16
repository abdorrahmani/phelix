package app

// IsWatched reports whether the identified application participates in
// backend monitoring/reporting (AppInfo.Watching). It resolves by app ID
// first, then by name — some reporters carry only the app name.
//
// An app that cannot be resolved is treated as watched. This is deliberate:
// `phelix remove` deletes the app from apps.json before reporting the
// "remove" event, and that tombstone must still reach the backend so a
// previously watched app is deleted from the dashboard too. Watching is a
// property of a registered app; an unregistered one has no watching state,
// and events about it are cleanup rather than monitoring data.
//
// This is the single gate the per-app reporting boundary consults. It reads
// the persisted state through Manager.ListApplications(), so a `phelix watch`
// toggle written by another process takes effect on the next reporting tick
// (at most one interval later) without restarting the monitor daemon.
func IsWatched(appID, appName string) bool {
	apps := Manager.ListApplications()

	if appID != "" {
		for _, a := range apps {
			if a.ID == appID {
				return a.Watching
			}
		}
	}
	if appName != "" {
		for _, a := range apps {
			if a.Name == appName {
				return a.Watching
			}
		}
	}
	return true
}

// AnyWatched reports whether at least one registered application is watched.
// It is the gate for server-level reporting — server identity, server
// metrics, self logs, agent metadata: the server transmits its own data to
// the backend only while it has at least one watched app to report alongside.
// A server whose every app has opted out sends no server data either.
//
// Like IsWatched it reads the persisted state through
// Manager.ListApplications(), so a `phelix watch` toggle written by another
// process takes effect on the monitor daemon's next reporting tick without a
// restart.
func AnyWatched() bool {
	for _, a := range Manager.ListApplications() {
		if a.Watching {
			return true
		}
	}
	return false
}
