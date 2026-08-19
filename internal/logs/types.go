package logs

import (
	"bufio"
	"os"
	"regexp"
	"sync"
	"time"
)

// MaxLogSize maximum size of log files
const MaxLogSize = 100 * 1024 * 1024 // 100 MB
const lastNOnColdStart = 5
const maxLinesPerTick = 200

// LogLevel is the severity of a log entry. It is a string so it can be sent
// verbatim over the wire (MonitorLogEntry.level) and matched against the
// on-disk `[LEVEL]` marker.
type LogLevel string

const (
	LevelInfo    LogLevel = "INFO"
	LevelWarning LogLevel = "WARNING"
	LevelError   LogLevel = "ERROR"
	LevelDebug   LogLevel = "DEBUG"
	// LevelWarn is a parse-tolerant alias for WARNING. Lines authored with
	// `[WARN]` are normalized to LevelWarning by parseLogLine.
	LevelWarn LogLevel = "WARN"
)

// LogStream identifies whether an application log line originated on the
// app's stdout or stderr. Self (daemon) logs always use StreamUnknown.
type LogStream string

const (
	StreamUnknown LogStream = ""
	StreamStdout  LogStream = "stdout"
	StreamStderr  LogStream = "stderr"
)

var (
	// timePrefix matches the timestamp every phelix-authored line starts with.
	timePrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`)
	// structuredLine matches a line authored by phelix's logging layers:
	//   "2006/01/02 15:04:05 [LEVEL] [stream-or-component] message"
	// The bracketed tag is either "stdout"/"stderr" (app logs) or a
	// component name (self logs). Unknown/legacy lines fall back to the
	// heuristic parse in parseLogLine.
	structuredLine = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})\s+\[(INFO|WARNING|WARN|ERROR|DEBUG)\]\s+\[([^\]]+)\]\s?(.*)$`)
)

// ParsedLog is the result of interpreting a single raw log line.
type ParsedLog struct {
	Level     LogLevel
	Message   string
	Raw       string
	Timestamp time.Time
	Stream    LogStream
	Component string
}

// LogEntry is a generic log output used by both app logs and self logs. It is
// the in-memory form of what is sent to the backend as MonitorLogEntry.
type LogEntry struct {
	ID        string    `json:"id"`
	ServerID  string    `json:"server_id"`
	AppID     string    `json:"app_id,omitempty"`
	Log       string    `json:"log"`
	Date      time.Time `json:"date"`
	Level     LogLevel  `json:"level"`
	Stream    LogStream `json:"stream,omitempty"`
	Component string    `json:"component,omitempty"`
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
