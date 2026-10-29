package logs

import (
	"time"
)

// CollectSelfLogs collects recent log entries from the local phelix.log file
// and any plain rotated archives of it.
func CollectSelfLogs(serverID string) ([]LogEntry, error) {
	path := SelfLogPath()

	var results []LogEntry
	var stored []LogRecord
	keys := make(map[string]bool)
	paths := append(PlainRotatedPaths(path, 0), path)
	for _, p := range paths {
		key := serverID + ":" + p
		keys[key] = true
		out, err := selfCollector.Collect(key, p, func(line string) LogEntry {
			entry, rec := selfEntryFromLine(line, serverID)
			stored = append(stored, rec)
			return entry
		})
		if err != nil {
			Error("logs", "failed to collect self logs from %s: %v", p, err)
			continue
		}
		results = append(results, out...)
	}
	selfCollector.Prune(func(key string) bool { return keys[key] })
	StoreRecords(stored)

	return results, nil
}

// selfEntryFromLine turns one raw phelix.log line into a LogEntry. JSON
// record lines written by the slog handler carry their own level and
// component; legacy text lines fall back to the marker parser.
func selfEntryFromLine(line, serverID string) (LogEntry, LogRecord) {
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
	}, rec
}

// RotateSelfLogs rotates the daemon's own log file when it exceeds the
// default threshold and reopens the writer onto the fresh file, so self logs
// keep flowing into the active path after the rotation.
func RotateSelfLogs() {
	rotated, err := RotateLogFile(SelfLogPath(), 0, 0, true)
	if err != nil {
		Error("logs", "failed to rotate self log: %v", err)
		return
	}
	if rotated {
		Info("logs", "rotated self log file")
		ReopenFileLog()
	}
}
