package app

import "github.com/abdorrahmani/phelix/internal/logs"

// LogTarget builds the log collector and rotation target for this app. The
// rotation fields only apply when the app enabled rotation explicitly;
// otherwise rotation runs with the documented defaults (100 MB, 5 files,
// gzip on). Compression defaults to on even for unconfigured apps: the
// setting is only meaningfully false when rotation was enabled and the
// app asked for plain archives.
func (a *AppInfo) LogTarget() logs.AppLogTarget {
	return buildLogTarget(a.ID, a.Name, a.Config().Logging)
}

// LogTarget builds the log collector and rotation target for one read-model
// list item. Same semantics as AppInfo.LogTarget.
func (a AppListItem) LogTarget() logs.AppLogTarget {
	return buildLogTarget(a.ID, a.Name, a.Logging)
}

// buildLogTarget maps the logging config onto the transport-agnostic target
// the logs package consumes.
func buildLogTarget(id, name string, l AppLoggingConfig) logs.AppLogTarget {
	t := logs.AppLogTarget{
		ID:       id,
		Name:     name,
		JSONLogs: l.JSONLogs,
		LogLevel: l.LogLevel,
		Compress: true,
	}
	if l.RotationEnabled {
		t.MaxSizeMB = l.RotationMaxSizeMB
		t.MaxFiles = l.RotationMaxFiles
		t.Compress = l.RotationCompress
	}
	return t
}
