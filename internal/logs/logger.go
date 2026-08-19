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

	logDir := filepath.Join(os.Getenv("HOME"), ".phelix", "logs")
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

// tees a message to both the log file and stderr. stderr keeps the daemon
// observable under a terminal/systemd, while the file is what gets shipped.
func writeLevel(level LogLevel, component, format string, args ...interface{}) {
	InitFileLog() // ensure file output exists before first write

	msg := fmt.Sprintf(format, args...)
	line := time.Now().Format(timeLayout) + " [" + string(level) + "] [" + component + "] " + msg

	logMu.Lock()
	if logFileHandle != nil {
		_, _ = io.WriteString(logFileHandle, line+"\n")
	}
	logMu.Unlock()

	// Mirror to stderr so operators see daemon activity on the terminal too.
	_, _ = fmt.Fprintln(os.Stderr, line)
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
