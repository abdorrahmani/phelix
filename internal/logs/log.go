package logs

import (
	"bufio"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/gophel/internal/server"
)

func RemoveSelfLogs() {
	logFile := filepath.Join(os.Getenv("HOME"), ".gophel", "logs", "gophel.log")

	info, err := os.Stat(logFile)
	if err != nil {
		log.Printf("ERROR: could not stat log file %s: %v", logFile, err)
		return
	}

	if info.Size() > MaxLogSize {
		log.Printf("WARNING: Gophel log file %s too large (%d bytes)", logFile, info.Size())
		err := trimLogFile(logFile)
		if err != nil {
			log.Printf("ERROR: Faild to remove log file %s: %v", logFile, err)
			return
		}
	}
}

func newSelfLogCollector() *selfLogCollector {
	return &selfLogCollector{
		selfLogReaders: make(map[string]*selfLogReader),
	}
}

var selfLogCollectorInstance = newSelfLogCollector()

// CollectSelfLogs collects self logs
func CollectSelfLogs() ([]SelfLog, error) {
	return selfLogCollectorInstance.collectSelfLogs()
}

// collectSelfLog reads new lines from Phelix self log file.
// On first run, it reads only the last N lines (cold start).
func (l *selfLogCollector) collectSelfLogs() ([]SelfLog, error) {
	serverID := server.GetServerID()
	logFile := filepath.Join(os.Getenv("HOME"), ".gophel", "logs", "gophel.log")

	var results []SelfLog

	l.mu.Lock()
	lr := l.selfLogReaders[serverID]
	l.mu.Unlock()

	if lr == nil {
		f, err := os.Open(logFile)
		if err != nil {
			l.mu.Unlock()
			return nil, err
		}
		lr = &selfLogReader{
			file:     f,
			reader:   bufio.NewReader(f),
			lastPos:  0,
			stopChan: make(chan struct{}),
		}

		if offset, err := findOffsetForLastNLines(f, lastNOnColdStart); err == nil {
			lr.lastPos = offset
		}
		l.mu.Lock()
		l.selfLogReaders[serverID] = lr
		l.mu.Unlock()
	}

	if _, err := lr.file.Seek(lr.lastPos, io.SeekStart); err != nil {
		return nil, err
	}
	lr.reader.Reset(lr.file)

	linesRead := 0
	for linesRead < maxLinesPerTick {
		line, err := lr.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		curPos, _ := lr.file.Seek(0, io.SeekCurrent)
		lr.lastPos = curPos

		results = append(results, SelfLog{
			ServerID: serverID,
			Log:      trimTrailingNewline(line),
			Date:     time.Now(),
		})
		linesRead++
	}

	return results, nil

}
