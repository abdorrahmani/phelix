package logs

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/server"
)

var appCollector = NewGenericLogCollector(
	maxLinesPerTick,
	lastNOnColdStart,
)

// CollectAppLogsUnified collects recent log lines from all registered applications
// and returns them as a unified slice of LogEntry structs.
func CollectAppLogsUnified() ([]LogEntry, error) {
	serverID := server.GetServerID()
	apps := app.Manager.ListApplications()

	var results []LogEntry

	for _, a := range apps {
		path := filepath.Join(
			os.Getenv("HOME"),
			".phelix", "logs",
			a.ID+".log",
		)

		logs, err := appCollector.Collect(
			a.ID,
			path,
			func(line string) LogEntry {
				p := parseLogLine(line)
				if p.Timestamp.IsZero() {
					p.Timestamp = time.Now()
				}
				return LogEntry{
					ID:       a.ID,
					ServerID: serverID,
					AppID:    a.ID,
					Log:      p.Message,
					Date:     p.Timestamp,
					Level:    p.Level,
				}
			},
		)

		if err != nil {
			results = append(results, logs...)
		}
	}

	return results, nil
}

// RemovePreviousLogs checks the size of the local apps file and trims it if it exceeds the maximum allowed size.
func RemovePreviousLogs() {
	apps := app.Manager.ListApplications()
	for _, appItem := range apps {
		logFile := filepath.Join(os.Getenv("HOME"), ".phelix", "logs", fmt.Sprintf("%s.log", appItem.ID))

		info, err := os.Stat(logFile)
		if err != nil {
			fmt.Printf("Failed to stat log file %s: %v", logFile, err)
			continue
		}

		if info.Size() > MaxLogSize {
			fmt.Printf("Log file %s too large (%d bytes)", logFile, info.Size())
			err := trimLogFile(logFile)
			if err != nil {
				fmt.Printf("Failed to remove log file %s: %v", logFile, err)
			}
		}
	}
}
