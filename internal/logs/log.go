package logs

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// openLog opens a log file for reading, creating it (and its parent logs
// directory) if it does not yet exist. A registered application may have no
// output yet, or its log file may have been deleted — in that case the failure
// mode should be a freshly created empty file, not a "no such file" error
// repeated on every collection tick.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
}

// NewGenericLogCollector creates a reusable log collector.
func NewGenericLogCollector(maxLines, coldLines int) *GenericLogCollector {
	return &GenericLogCollector{
		readers:   make(map[string]*readerState),
		maxLines:  maxLines,
		ColdLines: coldLines,
	}
}

// Collect reads new log lines from a given file key.
// key must be unique per logical log source (appID, serverID, etc).
//
// The collector tolerates the log file being recreated or truncated while it
// is held open: app restarts and log trimming write to the same path but can
// produce a new inode, so before reading we compare the open file against the
// path and reopen if the inode (or size) changed. Without this, the collector
// would keep reading the detached old file and silently stop seeing new app
// output.
func (c *GenericLogCollector) Collect(key, path string, mapper func(string) LogEntry) ([]LogEntry, error) {
	c.mu.Lock()
	rs := c.readers[key]
	c.mu.Unlock()

	if rs == nil {
		f, err := openLog(path)
		if err != nil {
			return nil, err
		}

		rs = &readerState{
			file:    f,
			reader:  bufio.NewReader(f),
			lastPos: 0,
		}

		if off, err := findOffsetForLastNLines(f, c.ColdLines); err == nil {
			rs.lastPos = off
		}

		c.mu.Lock()
		c.readers[key] = rs
		c.mu.Unlock()
	}

	if err := c.maybeReopen(rs, path); err != nil {
		return nil, err
	}

	if _, err := rs.file.Seek(rs.lastPos, io.SeekStart); err != nil {
		return nil, err
	}
	rs.reader.Reset(rs.file)

	var out []LogEntry
	lines := 0

	for lines < c.maxLines {
		line, err := rs.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		pos, _ := rs.file.Seek(0, io.SeekCurrent)
		rs.lastPos = pos

		out = append(out, mapper(trimTrailingNewline(line)))
		lines++
	}

	return out, nil
}

// maybeReopen checks whether the file at path still refers to the same inode
// as the collector's open handle (e.g. the log was trimmed, or the app was
// restarted and its log file recreated). If it changed, it closes the stale
// handle and reopens the new file, clamping the read position so we don't
// rescan lines that no longer exist.
func (c *GenericLogCollector) maybeReopen(rs *readerState, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		// The file is temporarily missing (e.g. mid-rotation). Leave the old
		// handle in place; a later Collect will retry the stat.
		return nil
	}

	fInfo, fErr := rs.file.Stat()
	if fErr != nil {
		// Handle is broken; force a reopen below by skipping the SameFile check.
	}

	if fErr == nil && os.SameFile(info, fInfo) {
		// Same inode. If it shrank underneath us (e.g. a truncate), clamp the
		// position so reads don't start past EOF.
		if rs.lastPos > info.Size() {
			rs.lastPos = info.Size()
		}
		return nil
	}

	f, err := openLog(path)
	if err != nil {
		return err
	}
	old := rs.file
	rs.file = f
	rs.reader = bufio.NewReader(f)

	if off, err := findOffsetForLastNLines(f, c.ColdLines); err == nil {
		rs.lastPos = off
	} else {
		rs.lastPos = 0
	}

	if old != nil {
		_ = old.Close()
	}
	return nil
}

// findOffsetForLastNLines finds the byte offset in the file where the last N lines begin.
// It scans backwards in chunks so a large (up to MaxLogSize) log file does not
// require one syscall per byte on a cold start.
//
// Line boundaries are newline positions; a file that does not end with a
// newline gets a virtual boundary at EOF (its last line is still complete).
// The wanted offset follows the boundary that has exactly N boundaries after
// it, or 0 when the file holds fewer than N+1 lines.
func findOffsetForLastNLines(f *os.File, n int) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() == 0 {
		return 0, nil
	}

	const chunkSize = 8 * 1024
	size := info.Size()

	last := make([]byte, 1)
	eofBoundary := false
	if _, err := f.ReadAt(last, size-1); err == nil {
		eofBoundary = last[0] != '\n'
	}

	count := 0
	pos := size
	buf := make([]byte, chunkSize)
	for pos > 0 {
		start := pos - chunkSize
		if start < 0 {
			start = 0
		}
		length := int(pos - start)
		if _, err := f.ReadAt(buf[:length], start); err != nil {
			return 0, err
		}
		for i := length - 1; i >= 0; i-- {
			isBoundary := buf[i] == '\n'
			if !isBoundary && pos == size && i == length-1 && eofBoundary {
				isBoundary = true // virtual boundary at unterminated EOF
			}
			if isBoundary {
				count++
				if count >= n+1 {
					return start + int64(i) + 1, nil
				}
			}
		}
		pos = start
	}
	return 0, nil
}

// trimTrailingNewline removes trailing newline characters (\n or \r) from the end of a string.
func trimTrailingNewline(s string) string {
	if len(s) == 0 {
		return s
	}
	if s[len(s)-1] == '\n' || s[len(s)-1] == '\r' {
		return trimTrailingNewline(s[:len(s)-1])
	}
	return s
}

// trimLogFile reduces the size of the log file to at most MaxLogSize by
// keeping the file's own inode: it rewrites the tail back into the same
// file. This matters because both the app process and the log collectors
// hold the file open across their lifetime — a rename-and-recreate approach
// would leave them attached to the detached old inode and they would stop
// seeing new output.
func trimLogFile(logFile string) error {
	info, err := os.Stat(logFile)
	if err != nil {
		return err
	}
	if info.Size() <= MaxLogSize {
		return nil
	}

	// Read the trailing MaxLogSize bytes.
	tail := info.Size() - MaxLogSize
	data := make([]byte, MaxLogSize)
	f, err := os.Open(logFile)
	if err != nil {
		return err
	}
	if _, err := f.ReadAt(data, tail); err != nil {
		f.Close()
		return err
	}
	f.Close()

	if len(data) > 0 && data[0] != '\n' {
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		}
	}

	// Rewrite in place, preserving the inode.
	w, err := os.OpenFile(logFile, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

var timeLayout = "2006/01/02 15:04:05"

// RecordFromLine interprets one raw log line. Lines written since the unified
// record format are JSON and parse directly; anything else falls back to the
// legacy text marker parser. ok is false only for blank lines.
func RecordFromLine(line string) (LogRecord, bool) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return LogRecord{}, false
	}

	if strings.HasPrefix(raw, "{") {
		var rec LogRecord
		if err := json.Unmarshal([]byte(raw), &rec); err == nil && rec.Source != "" {
			rec.Level = normalizeLevel(rec.Level)
			rec.Msg = strings.TrimSpace(rec.Msg)
			if rec.TS == "" {
				rec.TS = time.Now().UTC().Format(RFC3339Milli)
			}
			return rec, true
		}
	}

	p := parseLogLine(raw)
	rec := LogRecord{
		Level:     p.Level,
		Msg:       p.Message,
		Stream:    p.Stream,
		Component: p.Component,
	}
	if !p.Timestamp.IsZero() {
		rec.TS = p.Timestamp.UTC().Format(RFC3339Milli)
	}
	return rec, true
}

// parseLogLine interprets a legacy text line into a ParsedLog. Two shapes are
// understood: phelix-authored marker lines
// "2006/01/02 15:04:05 [LEVEL] [stream-or-component] message", whose level,
// stream/component and message are taken verbatim, and raw unmarked output
// whose level is inferred from keywords in the message. This parser exists
// only to read log files written before the unified record format; it is
// scheduled for removal once old files have rotated out.
func parseLogLine(line string) ParsedLog {
	raw := strings.TrimSpace(line)

	pl := ParsedLog{
		Raw:       raw,
		Timestamp: time.Time{},
		Level:     LevelInfo,
		Message:   raw,
		Stream:    StreamUnknown,
	}

	if len(raw) > len(timeLayout) && raw[len(timeLayout)] == ' ' {
		if t, err := time.Parse(timeLayout, raw[:len(timeLayout)]); err == nil {
			pl.Timestamp = t
			rest := strings.TrimSpace(raw[len(timeLayout):])
			if strings.HasPrefix(rest, "[") {
				if end := strings.IndexByte(rest, ']'); end > 0 {
					pl.Level = normalizeLevel(LogLevel(rest[1:end]))
					rest = strings.TrimSpace(rest[end+1:])
					if strings.HasPrefix(rest, "[") {
						if tagEnd := strings.IndexByte(rest, ']'); tagEnd > 0 {
							tag := strings.ToLower(strings.TrimSpace(rest[1:tagEnd]))
							if tag == string(StreamStdout) || tag == string(StreamStderr) {
								pl.Stream = LogStream(tag)
							} else {
								pl.Component = tag
							}
							rest = strings.TrimSpace(rest[tagEnd+1:])
						}
					}
					pl.Message = rest
					return pl
				}
			}
			pl.Message = rest
		}
	}

	pl.Level = textLevel(pl.Message)
	return pl
}

// textLevel infers a level from keywords in a raw message. This is the last
// resort for lines that carry no explicit level anywhere.
func textLevel(msg string) LogLevel {
	low := strings.ToLower(strings.TrimSpace(msg))
	switch {
	case strings.Contains(low, "error"),
		strings.Contains(low, "failed"),
		strings.Contains(low, "panic"):
		return LevelError

	case strings.Contains(low, "warn"),
		strings.Contains(low, "⚠"):
		return LevelWarning

	case strings.Contains(low, "debug"),
		strings.Contains(low, "[gin-debug]"):
		return LevelDebug

	default:
		return LevelInfo
	}
}

// normalizeLevel maps any level token found on disk or on the wire to the
// canonical lowercase spelling. Legacy lines carry uppercase markers
// ("INFO", "WARNING", "WARN", "ERROR", "DEBUG"); anything unrecognized is
// treated as info.
func normalizeLevel(l LogLevel) LogLevel {
	switch LogLevel(strings.ToLower(strings.TrimSpace(string(l)))) {
	case LevelDebug, "dbg":
		return LevelDebug
	case LevelWarning, "warning":
		return LevelWarning
	case LevelError, "err", "fatal":
		return LevelError
	default:
		return LevelInfo
	}
}

// levelRank orders levels for filtering: debug < info < warn < error. An
// unknown level ranks as info.
func levelRank(l LogLevel) int {
	switch normalizeLevel(l) {
	case LevelDebug:
		return 0
	case LevelWarning:
		return 2
	case LevelError:
		return 3
	default:
		return 1
	}
}
