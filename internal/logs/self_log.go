package logs

import (
	"os"
	"time"
)

// CollectSelfLogs collects recent log entries from the local phelix.log file.
func CollectSelfLogs(serverID string) ([]LogEntry, error) {
	path := SelfLogPath()

	return selfCollector.Collect(serverID, path, func(line string) LogEntry {
		p := parseLogLine(line)
		if p.Timestamp.IsZero() {
			p.Timestamp = time.Now()
		}

		return LogEntry{
			ID:        serverID,
			ServerID:  serverID,
			Log:       p.Message,
			Level:     p.Level,
			Date:      p.Timestamp,
			Stream:    p.Stream,
			Component: p.Component,
		}
	})
}

// RemoveSelfLogs checks the size of the local phelix.log file and trims it if
// it exceeds the maximum allowed size.
func RemoveSelfLogs() {
	logFile := SelfLogPath()

	info, err := os.Stat(logFile)
	if err != nil {
		Error("logs", "could not stat log file %s: %v", logFile, err)
		return
	}

	if info.Size() > MaxLogSize {
		Warning("logs", "phelix log file %s too large (%d bytes)", logFile, info.Size())
		if err := trimLogFile(logFile); err != nil {
			Error("logs", "failed to trim log file %s: %v", logFile, err)
			return
		}
	}
}
