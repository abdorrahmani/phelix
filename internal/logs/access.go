package logs

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

var (
	accessMu        sync.Mutex
	accessFile      *os.File
	accessCollector = NewGenericLogCollector(maxLinesPerTick, lastNOnColdStart)
)

// AccessLogPath returns the on-disk path of the proxy daemon's sampled
// access log: one JSON LogRecord per line with source "proxy".
func AccessLogPath() string {
	return filepath.Join(dataDir(), "logs", "access.log")
}

// NewRequestID returns a fresh 16-hex-character request identifier. It is
// echoed to the client in the X-Request-Id response header and stored on the
// access record, so a user reporting a problem can quote one id that finds
// the exact request.
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// AppendAccessRecord writes one proxy access record to the access log. The
// proxy runs as a separate process, so records reach the monitor daemon
// through this file: the monitor tails it and ships the entries to the
// backend inside the log batch.
func AppendAccessRecord(rec LogRecord) {
	rec.Source = SourceProxy
	line, err := rec.Encode()
	if err != nil {
		return
	}

	accessMu.Lock()
	defer accessMu.Unlock()
	if accessFile == nil {
		if err := os.MkdirAll(filepath.Dir(AccessLogPath()), 0o755); err != nil {
			return
		}
		f, err := os.OpenFile(AccessLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		accessFile = f
	}
	_, _ = accessFile.Write(append(line, '\n'))
}

// CloseAccessLog releases the access log handle. Called when the proxy
// daemon shuts down.
func CloseAccessLog() {
	accessMu.Lock()
	defer accessMu.Unlock()
	if accessFile != nil {
		_ = accessFile.Close()
		accessFile = nil
	}
}

// CollectAccessLogs tails the access log (and its plain rotations) for the
// monitor daemon and returns the new entries with the record's own
// correlation fields.
func CollectAccessLogs(serverID string) ([]LogEntry, error) {
	path := AccessLogPath()

	var results []LogEntry
	var stored []LogRecord
	keys := make(map[string]bool)
	paths := append(PlainRotatedPaths(path, 0), path)
	for _, p := range paths {
		key := "access:" + p
		keys[key] = true
		out, err := accessCollector.Collect(key, p, func(line string) LogEntry {
			entry, rec := accessEntryFromLine(line, serverID)
			stored = append(stored, rec)
			return entry
		})
		if err != nil {
			continue
		}
		results = append(results, out...)
	}
	accessCollector.Prune(func(key string) bool { return keys[key] })
	StoreRecords(stored)

	return results, nil
}

// accessEntryFromLine turns one raw access-log line into a LogEntry.
func accessEntryFromLine(line, serverID string) (LogEntry, LogRecord) {
	now := time.Now().UTC().Format(RFC3339Milli)
	rec, ok := RecordFromLine(line)
	if !ok {
		rec = LogRecord{TS: now, Level: LevelInfo}
	}
	rec.Source = SourceProxy
	if rec.TS == "" {
		rec.TS = now
	}
	return LogEntry{
		ID:        rec.AppID,
		ServerID:  serverID,
		AppID:     rec.AppID,
		App:       rec.App,
		Log:       rec.Msg,
		Date:      ParseRecordTime(rec.TS),
		Level:     rec.Level,
		RequestID: rec.RequestID,
		Component: rec.Component,
		Fields:    rec.Fields,
	}, rec
}
