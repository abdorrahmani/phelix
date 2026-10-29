package logs

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	logFileHandle *os.File
	logMu         sync.Mutex
)

// InitFileLog ensures self logs are written to ~/.phelix/logs/phelix.log and
// re-wires the global logger onto the same sink. Idempotent; safe to call
// from multiple packages during startup.
func InitFileLog() {
	logMu.Lock()
	defer logMu.Unlock()
	initFileLogLocked()
}

func initFileLogLocked() {
	if logFileHandle != nil {
		return
	}

	logDir := filepath.Join(dataDir(), "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
		return
	}

	f, err := os.OpenFile(filepath.Join(logDir, "phelix.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
		return
	}

	logFileHandle = f

	log.SetOutput(f)
	log.SetFlags(0)
}

// formatLevel renders a self-log line in the canonical structured format
// understood by the parser and the backend:
// "2006/01/02 15:04:05 [LEVEL] [component] message".
func formatLevel(level LogLevel, component, format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	return time.Now().Format(timeLayout) + " [" + string(level) + "] [" + component + "] " + msg
}

// writeLevelFile appends a self-log line to ~/.phelix/logs/phelix.log without
// echoing it to stderr. This is the transport path for short-lived CLI
// operations whose logs must reach the backend but must not clutter the
// user's terminal output.
func writeLevelFile(level LogLevel, component, format string, args ...any) {
	InitFileLog()

	line := formatLevel(level, component, format, args...)

	logMu.Lock()
	if logFileHandle != nil {
		_, _ = io.WriteString(logFileHandle, line+"\n")
	}
	logMu.Unlock()
}

// writeLevel writes a self-log line to both the log file and stderr. stderr
// keeps the daemon observable under a terminal/systemd, while the file is
// what gets shipped to the backend.
func writeLevel(level LogLevel, component, format string, args ...interface{}) {
	writeLevelFile(level, component, format, args...)
	_, _ = fmt.Fprintln(os.Stderr, formatLevel(level, component, format, args...))
}

// Info logs a level-INFO self message with the given component.
func Info(component, format string, args ...any) {
	writeLevel(LevelInfo, component, format, args...)
}

// Warning logs a level-WARNING self message with the given component.
func Warning(component, format string, args ...any) {
	writeLevel(LevelWarning, component, format, args...)
}

// Error logs a level-ERROR self message with the given component.
func Error(component, format string, args ...any) {
	writeLevel(LevelError, component, format, args...)
}

// Debug logs a level-DEBUG self message with the given component. DEBUG
// messages are written to the log file unconditionally — the file is the
// transport to the backend, not the terminal.
func Debug(component, format string, args ...any) {
	writeLevel(LevelDebug, component, format, args...)
}

// InfoFile logs a level-INFO self message to the log file only (no stderr
// echo). Used by short-lived CLI operations whose logs exist to reach the
// backend, not to be shown on the user's terminal.
func InfoFile(component, format string, args ...any) {
	writeLevelFile(LevelInfo, component, format, args...)
}

// WarningFile logs a level-WARNING self message to the log file only (no
// stderr echo). See InfoFile.
func WarningFile(component, format string, args ...any) {
	writeLevelFile(LevelWarning, component, format, args...)
}

// ErrorFile logs a level-ERROR self message to the log file only (no stderr
// echo). See InfoFile.
func ErrorFile(component, format string, args ...any) {
	writeLevelFile(LevelError, component, format, args...)
}

// DebugFile logs a level-DEBUG self message to the log file only (no stderr
// echo). See InfoFile.
func DebugFile(component, format string, args ...any) {
	writeLevelFile(LevelDebug, component, format, args...)
}
