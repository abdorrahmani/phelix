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

type LogLevel string

const (
	LevelInfo    LogLevel = "INFO"
	LevelWarning LogLevel = "WARNING"
	LevelError   LogLevel = "ERROR"
	LevelDebug   LogLevel = "DEBUG"
)

var (
	timePrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}`)
)

type ParsedLog struct {
	Level     LogLevel
	Message   string
	Raw       string
	Timestamp time.Time
}

// LogEntry is a generic log output used by both app logs and self logs.
type LogEntry struct {
	ID       string    `json:"id"`
	ServerID string    `json:"server_id"`
	AppID    string    `json:"app_id,omitempty"`
	Log      string    `json:"log"`
	Date     time.Time `json:"date"`
	Level    LogLevel  `json:"level"`
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
