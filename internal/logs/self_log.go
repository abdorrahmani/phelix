package logs

import (
	"os"
	"time"
)

// CollectSelfLogs collects recent log entries from the local phelix.log file.
func CollectSelfLogs(serverID string) ([]LogEntry, error) {
	path := SelfLogPath()

	return selfCollector.Collect(serverID, path, func(line string) LogEntry {
		return selfEntryFromLine(line, serverID)
	})
}

// selfEntryFromLine turns one raw phelix.log line into a LogEntry. JSON
// record lines written by the slog handler carry their own level and
// component; legacy text lines fall back to the marker parser.
func selfEntryFromLine(line, serverID string) LogEntry {
	now := time.Now().UTC().Format(RFC3339Milli)
	rec, ok := RecordFromLine(line)
	if !ok {
		rec = LogRecord{TS: now, Level: LevelInfo}
	}
	rec.Source = SourceSelf
	if rec.TS == "" {
		rec.TS = now
	}
	return LogEntry{
		ID:        serverID,
		ServerID:  serverID,
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
