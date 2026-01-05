package logs

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/abdorrahmani/gophel/internal/server"
)

func newAppLogCollector() *appLogCollector {
	return &appLogCollector{
		logReaders: make(map[string]*logReader),
	}
}

func (l *appLogCollector) SetLogCallback(callback func(logs AppLogs)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logCallback = callback
}

// singleton collector used by package-level CollectAppLogs
var defaultCollector = newAppLogCollector()

// CollectAppLogs reads new log lines for all apps since the previous call.
// It returns at most lastNOnColdStart lines on the first read per app, and
// only newly appended lines on subsequent calls.
func CollectAppLogs() ([]AppLogs, error) {
	return defaultCollector.collectAppLogs()
}

func (l *appLogCollector) collectAppLogs() ([]AppLogs, error) {
	serverID := server.GetServerID()
	apps := app.Manager.ListApplications()
	results := make([]AppLogs, 0)

	for _, appItem := range apps {
		logFile := filepath.Join(os.Getenv("HOME"), ".gophel", "logs", fmt.Sprintf("%s.log", appItem.ID))

		l.mu.Lock()
		lr := l.logReaders[appItem.ID]
		if lr == nil {
			f, err := os.Open(logFile)
			if err != nil {
				l.mu.Unlock()
				// Skip missing or unreadable files silently
				continue
			}
			lr = &logReader{
				appID:    appItem.ID,
				file:     f,
				reader:   bufio.NewReader(f),
				lastPos:  0,
				stopChan: make(chan struct{}),
			}
			// On cold start, set lastPos to the beginning of the last N lines
			offset, err := findOffsetForLastNLines(f, lastNOnColdStart)
			if err == nil {
				lr.lastPos = offset
			}
			l.logReaders[appItem.ID] = lr
		}
		// capture pointer and release lock before IO
		l.mu.Unlock()

		// Seek to last known position
		if _, err := lr.file.Seek(lr.lastPos, io.SeekStart); err != nil {
			continue
		}
		lr.reader.Reset(lr.file)

		linesRead := 0
		for linesRead < maxLinesPerTick {
			line, err := lr.reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					break
				}
				break
			}
			curPos, _ := lr.file.Seek(0, io.SeekCurrent)
			lr.lastPos = curPos
			results = append(results, AppLogs{
				ID:       appItem.ID,
				ServerID: serverID,
				AppID:    appItem.ID,
				Log:      trimTrailingNewline(line),
				Date:     time.Now(),
			})
			linesRead++
		}
	}

	return results, nil
}

func findOffsetForLastNLines(f *os.File, n int) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() == 0 {
		return 0, nil
	}

	var count int
	var pos = info.Size() - 1
	buf := make([]byte, 1)
	for pos >= 0 {
		if _, err := f.ReadAt(buf, pos); err != nil {
			return 0, err
		}
		if buf[0] == '\n' {
			count++
			if count >= n+1 { // position to start of last N lines
				return pos + 1, nil
			}
		}
		pos--
	}
	return 0, nil
}

func trimTrailingNewline(s string) string {
	if len(s) == 0 {
		return s
	}
	if s[len(s)-1] == '\n' || s[len(s)-1] == '\r' {
		return trimTrailingNewline(s[:len(s)-1])
	}
	return s
}

// RemovePreviousLogs remove logs of each application that run with gophel by size (100 MB)
func RemovePreviousLogs() {
	apps := app.Manager.ListApplications()
	for _, appItem := range apps {
		logFile := filepath.Join(os.Getenv("HOME"), ".gophel", "logs", fmt.Sprintf("%s.log", appItem.ID))

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

// trimLogFile trim logs file of application by size (100 MB)
func trimLogFile(logFile string) error {
	file, err := os.Open(logFile)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	if info.Size() <= MaxLogSize {
		return nil
	}

	start := info.Size() - MaxLogSize
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return err
	}

	tempPath := logFile + ".tmp"
	tempFile, err := os.Create(tempPath)
	if err != nil {
		return err
	}
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		return err
	}

	return os.Rename(tempPath, logFile)
}
