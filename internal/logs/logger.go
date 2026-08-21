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

// This file provides the standardized, leveled logger every part of the
// Phelix agent uses for its own ("self") logs. It achieves two goals:
//
//  1. Consistency — every self log line is written in the same structured
//     format the parser understands and the backend displays:
//     "2006/01/02 15:04:05 [LEVEL] [component] message"
//
//  2. Delivery — self logs must reach the backend. They do so by being
//     written to ~/.phelix/logs/phelix.log, which the monitor daemon tails
//     and forwards as MonitorLogEntry{source: LOG_SOURCE_SELF}. The global
//     Go "log" package is pointed at the same file so even code that still
//     calls log.Printf lands in the same place (assigned [INFO] [misc]).
//
// Two write paths exist:
//
//   - Info/Warning/Error/Debug — write to the log file AND mirror to stderr.
//     This is for daemon-level activity an operator running `phelix monitor`
//     in the foreground (or under systemd) should see on the terminal.
//
//   - InfoFile/WarningFile/ErrorFile/DebugFile — write to the log file ONLY.
//     This is for short-lived CLI operations whose self-logs exist to reach
//     the backend (event reporting, version sync, …). Echoing them to stderr
//     would pollute the user's command output, so they are kept off the
//     terminal.
//
// Use Info/Warning/Error/Debug(component, format, args...) throughout. Keep
// the component short and stable — it is what the backend groups self logs
// by. See the registry in docs/logging-backend.md.

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
		// Fall back to stderr only — the daemon still needs its diagnostics
		// somewhere, and self-log delivery degrades to the terminal.
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

	// Global logger → the same file, with flags off: we already add our own
	// timestamp, so left-over log.Printf call sites shouldn't double-prefix.
	log.SetOutput(f)
	log.SetFlags(0)
}

// formatLevel renders a self-log line in the canonical structured format
// understood by the parser and the backend:
// "2006/01/02 15:04:05 [LEVEL] [component] message".
func formatLevel(level LogLevel, component, format string, args ...interface{}) string {
	msg := fmt.Sprintf(format, args...)
	return time.Now().Format(timeLayout) + " [" + string(level) + "] [" + component + "] " + msg
}

// writeLevelFile appends a self-log line to ~/.phelix/logs/phelix.log without
// echoing it to stderr. This is the transport path for short-lived CLI
// operations whose logs must reach the backend but must not clutter the
// user's terminal output.
func writeLevelFile(level LogLevel, component, format string, args ...interface{}) {
	InitFileLog() // ensure file output exists before first write

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

	// Mirror to stderr so operators see daemon activity on the terminal too.
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
