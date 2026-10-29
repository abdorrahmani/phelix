package logs

import (
	"os"
	"path/filepath"
	"time"
)

// dataDir is the persistent state directory for the Phelix runtime. It must
// match the resolution used by the app manager and server packages, because
// app logs are written under it (see internal/app/state.go's runtimeDataDir)
// and this package reads them back from the same location. Docker deployments
// use /var/lib/phelix so mounting that path preserves state across container
// recreation; local installations retain ~/.phelix unless PHELIX_DATA_DIR
// overrides it.
func dataDir() string {
	if dir := os.Getenv("PHELIX_DATA_DIR"); dir != "" {
		return dir
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "/var/lib/phelix"
	}
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE")
	}
	return filepath.Join(homeDir, ".phelix")
}

// builtinCollectors are the transport-agnostic collectors used by the monitor
// daemon. They are package-level singletons because the monitor's metrics
// collector is itself a process-wide singleton and must keep reading from
// where it left off across ticks.
var (
	appCollector  = NewGenericLogCollector(maxLinesPerTick, lastNOnColdStart)
	selfCollector = NewGenericLogCollector(maxLinesPerTick, lastNOnColdStart)
)

// AppLogTarget identifies one managed application whose log file should be
// collected. Paths are derived from the app ID (see AppLogPath), keeping the
// logs package free of any dependency on the app manager. The caller
// enriches targets with what the log file itself cannot carry: the app's
// human-readable name, its serving version, and its logging configuration.
type AppLogTarget struct {
	ID        string
	Name      string
	Version   string
	JSONLogs  bool
	LogLevel  string
	MaxSizeMB int
	MaxFiles  int
	Compress  bool
}

// AppLogPath returns the on-disk path for an application's combined
// (stdout+stderr) log file.
func AppLogPath(id string) string {
	return filepath.Join(dataDir(), "logs", id+".log")
}

// SelfLogPath returns the on-disk path for the daemon's own log file.
func SelfLogPath() string {
	return filepath.Join(dataDir(), "logs", "phelix.log")
}

// CollectAppLogs collects new log lines from every target application's log
// files and returns them as a unified slice. Each app is read from its active
// file and from any plain rotated file: after a rotation the app keeps
// writing into the renamed file until it restarts, so both are live sources.
// Failures to read a single app are logged and skipped — one broken app must
// not block the others.
func CollectAppLogs(targets []AppLogTarget, serverID string) ([]LogEntry, error) {
	var results []LogEntry
	keys := make(map[string]bool)

	for _, t := range targets {
		logFile := AppLogPath(t.ID)

		minRank := -1
		if t.LogLevel != "" {
			minRank = levelRank(LogLevel(t.LogLevel))
		}
		paths := append(PlainRotatedPaths(logFile, t.MaxFiles), logFile)
		for _, p := range paths {
			key := t.ID + ":" + p
			keys[key] = true
			out, err := appCollector.Collect(key, p, func(line string) LogEntry {
				return appEntryFromLine(t, line, serverID)
			})
			if err != nil {
				Error("logs", "failed to collect logs for app %s: %v", t.ID, err)
				continue
			}
			for _, e := range out {
				if minRank >= 0 && levelRank(e.Level) < minRank {
					continue
				}
				results = append(results, e)
			}
		}
	}
	appCollector.Prune(func(key string) bool { return keys[key] })

	return results, nil
}

// appEntryFromLine turns one raw line from an app's log file into a LogEntry.
// JSON output (Go slog, Rust tracing) carries the app's real level and
// correlation fields; plain text falls back to explicit markers, the stream
// default and the keyword heuristics, in that order. The target's name and
// version enrich every line: the file only stores the app id, and the
// human-readable name plus the serving version are what the dashboard groups
// by.
func appEntryFromLine(t AppLogTarget, line, serverID string) LogEntry {
	now := time.Now()
	rec, ok := detectAppRecord(line, t.JSONLogs, now)
	if !ok {
		rec = LogRecord{TS: now.UTC().Format(RFC3339Milli), Level: LevelInfo}
	}
	rec.Source = SourceApp
	rec.App = t.Name
	rec.AppID = t.ID
	if rec.TS == "" {
		rec.TS = now.UTC().Format(RFC3339Milli)
	}
	if rec.Version == "" {
		rec.Version = t.Version
	}
	return LogEntry{
		ID:        t.ID,
		ServerID:  serverID,
		AppID:     t.ID,
		App:       rec.App,
		Log:       rec.Msg,
		Date:      ParseRecordTime(rec.TS),
		Level:     rec.Level,
		Stream:    rec.Stream,
		Component: rec.Component,
		Version:   rec.Version,
		DeployID:  rec.DeployID,
		RequestID: rec.RequestID,
		Fields:    rec.Fields,
	}
}

// RotateAppLogs rotates every given app's log file when it exceeds its
// configured size, keeping gzip archives of older rotations. Targets come
// from the caller (the app manager) to keep this package free of app
// imports.
func RotateAppLogs(targets []AppLogTarget) {
	for _, t := range targets {
		logFile := AppLogPath(t.ID)
		rotated, err := RotateLogFile(logFile, t.MaxSizeMB, t.MaxFiles, t.Compress)
		if err != nil {
			Error("logs", "failed to rotate log file %s: %v", logFile, err)
			continue
		}
		if rotated {
			Info("logs", "rotated log file %s", logFile)
		}
	}
}
