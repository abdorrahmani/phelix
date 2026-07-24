package grpc

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	sharedLogger     *log.Logger
	sharedLoggerOnce sync.Once
	sharedLogFile    *os.File
)

// initSharedLogger initializes a logger that writes to the shared phelix.log file.
func initSharedLogger() {
	sharedLoggerOnce.Do(func() {
		logDir := filepath.Join(os.Getenv("HOME"), ".phelix", "logs")
		if err := os.MkdirAll(logDir, 0755); err != nil {
			// Fall back to stderr if we can't create the log directory
			sharedLogger = log.New(os.Stderr, "", log.LstdFlags)
			return
		}

		logPath := filepath.Join(logDir, "phelix.log")
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			sharedLogger = log.New(os.Stderr, "", log.LstdFlags)
			return
		}

		sharedLogFile = f
		sharedLogger = log.New(f, "", 0)
	})
}

// grpcLog writes a message to the shared phelix.log file.
func grpcLog(format string, args ...interface{}) {
	initSharedLogger()
	msg := fmt.Sprintf(format, args...)
	sharedLogger.Printf("%s %s\n", time.Now().Format("2006/01/02 15:04:05"), msg)
}
