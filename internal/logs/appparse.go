package logs

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// detectAppRecord builds a LogRecord from one raw line of managed app output.
// JSON lines (Go slog, Rust tracing) yield the app's real level, timestamp
// and correlation fields; plain text lines yield an explicit level marker
// when the app printed one, and the stream/keyword inference otherwise. The
// default behavior of unmarked text is unchanged: info, or the keyword
// heuristics for error-ish words.
func detectAppRecord(line string, jsonMode bool, now time.Time) (LogRecord, bool) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return LogRecord{}, false
	}

	if jsonMode || strings.HasPrefix(raw, "{") {
		if rec, ok := appJSONRecord(raw); ok {
			return rec, true
		}
		if jsonMode {
			return LogRecord{TS: now.UTC().Format(RFC3339Milli), Level: LevelInfo, Msg: raw}, true
		}
	}

	return detectTextRecord(raw), true
}

// appJSONRecord interprets one line as an application JSON record. A line
// only counts as a record when it carries at least one known message or
// level key; arbitrary JSON payloads the app prints stay raw text so their
// content is never silently swallowed into fields.
func appJSONRecord(raw string) (LogRecord, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return LogRecord{}, false
	}

	rec := LogRecord{Level: LevelInfo, Stream: StreamUnknown}
	msgKey, _ := firstKey(m, "msg", "message")
	lvlKey, _ := firstKey(m, "level", "severity")
	if msgKey == "" && lvlKey == "" {
		return LogRecord{}, false
	}

	if lvlKey != "" {
		rec.Level = normalizeLevel(LogLevel(valueString(m[lvlKey])))
	}
	if msgKey != "" {
		rec.Msg = valueString(m[msgKey])
	}
	var nested map[string]any
	if f, ok := firstKey(m, "fields"); ok {
		if obj, ok := m[f].(map[string]any); ok {
			nested = obj
		}
	}
	if rec.Msg == "" {
		if nested != nil {
			if v, ok := nested["message"]; ok {
				rec.Msg = valueString(v)
				delete(nested, "message")
			}
		}
		if rec.Msg == "" {
			rec.Msg = raw
		}
	}
	if v, ok := firstKey(m, "time", "ts", "timestamp"); ok {
		if t := ParseRecordTime(valueString(m[v])); !t.IsZero() {
			rec.TS = t.UTC().Format(RFC3339Milli)
		}
	}
	if rec.RequestID == "" {
		if v, ok := firstKey(m, "request_id", "trace_id"); ok {
			rec.RequestID = valueString(m[v])
		} else if nested != nil {
			if v, ok := firstKey(nested, "request_id", "trace_id"); ok {
				rec.RequestID = valueString(nested[v])
				delete(nested, v)
			}
		}
	}

	consumed := map[string]bool{msgKey: true, lvlKey: true}
	if _, ok := firstKey(m, "fields"); ok {
		consumed["fields"] = true
	}
	for k, v := range m {
		if k == "" || consumed[k] {
			continue
		}
		if rec.Fields == nil {
			rec.Fields = make(map[string]string)
		}
		rec.Fields[k] = valueString(v)
	}
	for k, v := range nested {
		if k == "" {
			continue
		}
		if rec.Fields == nil {
			rec.Fields = make(map[string]string)
		}
		rec.Fields[k] = valueString(v)
	}
	return rec, true
}

// firstKey returns the value for the first key present in m.
func firstKey(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return k, true
		}
	}
	return "", false
}

// detectTextRecord interprets one plain-text app line: an explicit level
// marker when the app printed one (level=warn, [WARN], a leading WARN or
// ERROR word), the legacy marker/keyword inference otherwise.
func detectTextRecord(raw string) LogRecord {
	if lvl, ok := explicitLevel(raw); ok {
		rec := LogRecord{Level: lvl, Msg: raw}
		if t, rest := leadingTime(raw); !t.IsZero() {
			rec.TS = t.UTC().Format(RFC3339Milli)
			rec.Msg = strings.TrimSpace(rest)
		}
		return rec
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
	return rec
}

// explicitLevel extracts an explicit level marker from text output and
// reports whether one was found.
func explicitLevel(msg string) (LogLevel, bool) {
	low := strings.ToLower(msg)
	if i := strings.Index(low, "level="); i >= 0 {
		rest := msg[i+len("level="):]
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		tok := strings.Trim(rest[:end], "\"[]")
		if lvl, ok := levelToken(tok); ok {
			return lvl, true
		}
	}
	if strings.HasPrefix(msg, "[") {
		if end := strings.IndexByte(msg, ']'); end > 0 {
			if lvl, ok := levelToken(msg[1:end]); ok {
				return lvl, true
			}
		}
	}
	first := msg
	if i := strings.IndexAny(msg, " \t"); i >= 0 {
		first = msg[:i]
	}
	if lvl, ok := levelToken(strings.TrimSuffix(first, ":")); ok {
		return lvl, true
	}
	return LevelInfo, false
}

// levelToken maps one level word to the canonical spelling, reporting false
// for words that are not level names.
func levelToken(tok string) (LogLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(tok)) {
	case "debug", "dbg", "trace":
		return LevelDebug, true
	case "info", "information", "notice":
		return LevelInfo, true
	case "warn", "warning":
		return LevelWarning, true
	case "error", "err", "fatal", "critical", "crit", "panic":
		return LevelError, true
	}
	return LevelInfo, false
}

// leadingTime parses a leading human timestamp prefix from a text line.
func leadingTime(raw string) (time.Time, string) {
	if len(raw) <= len(timeLayout) || raw[len(timeLayout)] != ' ' {
		return time.Time{}, raw
	}
	t, err := time.Parse(timeLayout, raw[:len(timeLayout)])
	if err != nil {
		return time.Time{}, raw
	}
	return t, raw[len(timeLayout):]
}

// valueString renders one JSON value as a flat string for the fields object.
// Nested structures keep their JSON shape so no information is invented.
func valueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}
