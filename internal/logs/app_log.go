package logs

import (
	"os"
	"path/filepath"
	"time"
)

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
// logs package free of any dependency on the app manager.
type AppLogTarget struct {
	ID string
}

// AppLogPath returns the on-disk path for an application's combined
// (stdout+stderr) log file.
func AppLogPath(id string) string {
	return filepath.Join(os.Getenv("HOME"), ".phelix", "logs", id+".log")
}

// SelfLogPath returns the on-disk path for the daemon's own log file.
func SelfLogPath() string {
	return filepath.Join(os.Getenv("HOME"), ".phelix", "logs", "phelix.log")
}

// CollectAppLogs collects new log lines from every target application's log
// file and returns them as a unified slice. Failures to read a single app are
// logged and skipped — one broken app must not block the others.
func CollectAppLogs(targets []AppLogTarget, serverID string) ([]LogEntry, error) {
	var results []LogEntry

	for _, t := range targets {
		logFile := AppLogPath(t.ID)

		out, err := appCollector.Collect(t.ID, logFile, func(line string) LogEntry {
			p := parseLogLine(line)
			if p.Timestamp.IsZero() {
				p.Timestamp = time.Now()
			}
			return LogEntry{
				ID:        t.ID,
				ServerID:  serverID,
				AppID:     t.ID,
				Log:       p.Message,
				Date:      p.Timestamp,
				Level:     p.Level,
				Stream:    p.Stream,
				Component: p.Component,
			}
		})
		if err != nil {
			// The file may not exist yet (app started but emitted nothing), or
			// was deleted. Skip this app rather than failing the whole batch.
			Error("logs", "failed to collect logs for app %s: %v", t.ID, err)
			continue
		}
		results = append(results, out...)
	}

	return results, nil
}

// RemovePreviousLogs checks the size of each given app's local log file and
// trims it if it exceeds the maximum allowed size. Targets come from the
// caller (the app manager) to keep this package free of app imports.
func RemovePreviousLogs(targets []AppLogTarget) {
	for _, t := range targets {
		logFile := AppLogPath(t.ID)

		info, err := os.Stat(logFile)
		if err != nil {
			Error("logs", "failed to stat log file %s: %v", logFile, err)
			continue
		}

		if info.Size() > MaxLogSize {
			Info("logs", "trimming oversized log file %s (%d bytes)", logFile, info.Size())
			if err := trimLogFile(logFile); err != nil {
				Error("logs", "failed to trim log file %s: %v", logFile, err)
			}
		}
	}
}
