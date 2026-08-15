package auth

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const logFile = "phelix.log"

var logFileHandle *os.File

// setupLogging initializes a file-based logger under ~/.phelix/logs.
func setupLogging() {
	logDir := filepath.Join(os.Getenv("HOME"), ".phelix", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		fmt.Printf("⚠ Failed to create log directory: %s\n", logDir)
		return
	}

	logPath := filepath.Join(logDir, logFile)
	var err error
	logFileHandle, err = os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("⚠ Failed to open log file: %v\n", err)
		return
	}

	log.SetOutput(logFileHandle)
}
