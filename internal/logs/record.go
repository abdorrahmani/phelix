package logs

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// RFC3339Milli is the on-disk timestamp layout for every LogRecord: RFC3339
// with fixed millisecond precision, always UTC, so lines from all three
// producers (self, app, proxy) sort chronologically by plain string compare.
const RFC3339Milli = "2006-01-02T15:04:05.000Z07:00"

// maxRecordMsg caps a record's message length. Application output is
// untrusted and unbounded; a single runaway line must not dominate a log file
// or a wire batch.
const maxRecordMsg = 8192

// Log record sources. Every producer of a LogRecord declares where the line
// came from; consumers group and filter on it.
const (
	SourceSelf   = "self"
	SourceApp    = "app"
	SourceProxy  = "proxy"
	SourceBuild  = "build"
	SourceDeploy = "deploy"
)

// LogRecord is the single logging format shared by every Phelix layer. It is
// written to disk as one JSON object per line and read back with
// json.Unmarshal; no regex parses a LogRecord line. The fields object carries
// extra structured data and must never hold secrets or environment variables:
// Encode redacts msg and every field value through phelixerr.Redact, but the
// first line of defense is that producers never put sensitive material in.
type LogRecord struct {
	TS        string            `json:"ts"`
	Level     LogLevel          `json:"level"`
	Source    string            `json:"source"`
	App       string            `json:"app,omitempty"`
	AppID     string            `json:"app_id,omitempty"`
	Component string            `json:"component,omitempty"`
	Version   string            `json:"version,omitempty"`
	DeployID  string            `json:"deploy_id,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
	Stream    LogStream         `json:"stream,omitempty"`
	Msg       string            `json:"msg"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// NewRecord stamps a record with the current UTC time and canonical level.
func NewRecord(source string, level LogLevel, msg string) LogRecord {
	return LogRecord{
		TS:     time.Now().UTC().Format(RFC3339Milli),
		Level:  level,
		Source: source,
		Msg:    msg,
	}
}

// Encode returns one JSON line for the record with the message capped and
// credential material redacted out of the message and every field value. The
// receiver is never modified.
func (r LogRecord) Encode() ([]byte, error) {
	return json.Marshal(r.sanitized())
}

// sensitiveFieldKeys are field names that never travel in a record's fields
// object, whatever their value. Redaction patterns can only catch credential
// SHAPES inside a value; a bare secret stored under a key like "password" or
// "token" is only catchable by name. Matching is case-insensitive against
// the exact key and against key substrings containing one of these words.
var sensitiveFieldKeys = []string{
	"password", "passwd", "secret", "token", "authorization", "auth",
	"api_key", "apikey", "private_key", "cookie", "credentials", "session",
}

// sanitized returns a copy with sensitive-named fields dropped, the message
// capped to maxRecordMsg, and redaction applied to the message and every
// remaining field value.
func (r LogRecord) sanitized() LogRecord {
	out := r
	out.Msg = capMsg(phelixerr.Redact(r.Msg))
	if len(r.Fields) > 0 {
		fields := make(map[string]string, len(r.Fields))
		for k, v := range r.Fields {
			if sensitiveFieldKey(k) {
				continue
			}
			fields[k] = capMsg(phelixerr.Redact(v))
		}
		out.Fields = fields
	}
	return out
}

// sensitiveFieldKey reports whether a field name looks like it carries
// credential material.
func sensitiveFieldKey(key string) bool {
	low := strings.ToLower(key)
	low = strings.ReplaceAll(low, "-", "_")
	low = strings.ReplaceAll(low, " ", "_")
	for _, bad := range sensitiveFieldKeys {
		if low == bad {
			return true
		}
	}
	return false
}

// textLine renders the human-readable terminal view of a record, in the
// legacy "2006/01/02 15:04:05 [LEVEL] [component] message" shape operators
// already know, converted to local time.
func (r LogRecord) textLine() string {
	s := r.sanitized()
	t, err := time.Parse(RFC3339Milli, s.TS)
	if err != nil {
		t = time.Now()
	}
	level := strings.ToUpper(string(s.Level))
	tag := s.Component
	if s.Source != SourceSelf {
		tag = s.Source
		if s.App != "" {
			tag = s.Source + ":" + s.App
		}
	}
	return t.Local().Format(timeLayout) + " [" + level + "] [" + tag + "] " + s.Msg
}

// capMsg truncates a string to maxRecordMsg bytes without splitting a UTF-8
// rune in half.
func capMsg(s string) string {
	if len(s) <= maxRecordMsg {
		return s
	}
	cut := s[:maxRecordMsg]
	for {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			return cut
		}
		cut = cut[:len(cut)-1]
	}
}

// ParseRecordTime parses an on-disk ts value; the zero time is returned when
// the value is missing or malformed.
func ParseRecordTime(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	t, err := time.Parse(RFC3339Milli, ts)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, ts); err != nil {
			return time.Time{}
		}
	}
	return t
}
