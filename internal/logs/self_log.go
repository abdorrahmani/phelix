package logs

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/server"
)

var selfCollector = NewGenericLogCollector(
	maxLinesPerTick,
	lastNOnColdStart,
)

// CollectSelfLogsUnified collects recent log entries from the local phelix.log file.
func CollectSelfLogsUnified() ([]LogEntry, error) {
	serverID := server.GetServerID()
	path := filepath.Join(
		os.Getenv("HOME"),
		".phelix", "logs",
		"phelix.log",
	)

	return selfCollector.Collect(
		serverID,
		path,
		func(line string) LogEntry {
			p := parseLogLine(line)
			if p.Timestamp.IsZero() {
				p.Timestamp = time.Now()
			}

			return LogEntry{
				ID:       serverID,
				ServerID: serverID,
				Log:      p.Message,
				Level:    p.Level,
				Date:     p.Timestamp,
			}
		})

}

// RemoveSelfLogs checks the size of the local phelix.log file and trims it if it exceeds the maximum allowed size.
func RemoveSelfLogs() {
	logFile := filepath.Join(os.Getenv("HOME"), ".phelix", "logs", "phelix.log")

	info, err := os.Stat(logFile)
	if err != nil {
		log.Printf("ERROR: could not stat log file %s: %v", logFile, err)
		return
	}

	if info.Size() > MaxLogSize {
		log.Printf("WARNING: Phelix log file %s too large (%d bytes)", logFile, info.Size())
		err := trimLogFile(logFile)
		if err != nil {
			log.Printf("ERROR: Faild to remove log file %s: %v", logFile, err)
			return
		}
	}
}
