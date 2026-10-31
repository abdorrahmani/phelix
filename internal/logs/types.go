package logs

import (
	"bufio"
	"os"
	"sync"
	"time"
)

// MaxLogSize maximum size of log files
const MaxLogSize = 100 * 1024 * 1024 // 100 MB
const lastNOnColdStart = 5
const maxLinesPerTick = 200

// LogLevel is the severity of a log entry. It is a string so it can be sent
// verbatim over the wire (MonitorLogEntry.level) and stored verbatim in a
// LogRecord. The canonical spellings are lowercase: debug, info, warn, error.
// Legacy lines carrying uppercase markers are normalized by normalizeLevel.
type LogLevel string

const (
	LevelDebug   LogLevel = "debug"
	LevelInfo    LogLevel = "info"
	LevelWarning LogLevel = "warn"
	LevelError   LogLevel = "error"
)

// LogStream identifies whether an application log line originated on the
// app's stdout or stderr. Self (daemon) logs always use StreamUnknown.
type LogStream string

const (
	StreamUnknown LogStream = ""
	StreamStdout  LogStream = "stdout"
	StreamStderr  LogStream = "stderr"
)

// ParsedLog is the result of interpreting a single raw legacy text line.
type ParsedLog struct {
	Level     LogLevel
	Message   string
	Raw       string
	Timestamp time.Time
	Stream    LogStream
	Component string
}

// LogEntry is a generic log output used by app logs, self logs and proxy
// access logs. It is the in-memory form of what is sent to the backend as
// MonitorLogEntry: the legacy identity fields plus the unified record's
// correlation fields.
type LogEntry struct {
	ID        string            `json:"id"`
	ServerID  string            `json:"server_id"`
	AppID     string            `json:"app_id,omitempty"`
	App       string            `json:"app,omitempty"`
	Log       string            `json:"log"`
	Date      time.Time         `json:"date"`
	Level     LogLevel          `json:"level"`
	Stream    LogStream         `json:"stream,omitempty"`
	Component string            `json:"component,omitempty"`
	Version   string            `json:"version,omitempty"`
	DeployID  string            `json:"deploy_id,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// readerState keeps file cursor and reader state.
type readerState struct {
	file    *os.File
	reader  *bufio.Reader
	lastPos int64
}

// GenericLogCollector manages multiple log readers safely.
type GenericLogCollector struct {
	mu        sync.Mutex
	readers   map[string]*readerState
	maxLines  int
	ColdLines int
}
