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
