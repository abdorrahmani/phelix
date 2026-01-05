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

// LogEntry is a generic log output used by both app logs and self logs.
type LogEntry struct {
	ID       string    `json:"id"`
	ServerID string    `json:"server_id"`
	AppID    string    `json:"app_id,omitempty"`
	Log      string    `json:"log"`
	Date     time.Time `json:"date"`
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
