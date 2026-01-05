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

// AppLogs represents the application log
type AppLogs struct {
	ID       string    `json:"id"`
	ServerID string    `json:"server_id"`
	AppID    string    `json:"app_id"`
	Log      string    `json:"log"`
	Date     time.Time `json:"date"`
}

// logReader represents the reader
type logReader struct {
	appID     string
	file      *os.File
	reader    *bufio.Reader
	lastPos   int64
	stopChan  chan struct{}
	isRunning bool
}

// appLogCollector represents log collector
type appLogCollector struct {
	logReaders  map[string]*logReader
	mu          sync.RWMutex
	logCallback func(AppLogs)
}

type selfLogCollector struct {
	selfLogReaders map[string]*selfLogReader
	mu             sync.RWMutex
	logCallback    func(AppLogs)
}

type SelfLog struct {
	ServerID string    `json:"server_id"`
	Log      string    `json:"log"`
	Date     time.Time `json:"date"`
}

type selfLogReader struct {
	file     *os.File
	reader   *bufio.Reader
	lastPos  int64
	stopChan chan struct{}
}
