package logs

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	logFileHandle *os.File
	logMu         sync.Mutex

	selfEchoHandler slog.Handler
	selfFileHandler slog.Handler
)

// componentKey is the slog attribute holding the self-log component; every
// other attribute lands in the record's fields object.
const componentKey = "component"

// InitFileLog ensures self logs are written to ~/.phelix/logs/phelix.log and
// wires the default slog logger (and through it the standard log package)
// onto the same sink. Idempotent; safe to call from multiple packages during
// startup.
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
	if err := os.MkdirAll(logDir, 0755); err == nil {
		if f, ferr := os.OpenFile(filepath.Join(logDir, "phelix.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); ferr == nil {
			logFileHandle = f
		}
	}

	selfEchoHandler = &selfHandler{file: logFileHandle, echo: true}
	selfFileHandler = &selfHandler{file: logFileHandle}
	slog.SetDefault(slog.New(selfEchoHandler))
	log.SetFlags(0)
}

// selfHandler is the slog.Handler behind every self log. It writes each
// record as one JSON LogRecord line to phelix.log and, when echo is set,
// mirrors a human-readable line to stderr so a daemon running in the
// foreground or under systemd stays observable.
type selfHandler struct {
	file  io.Writer
	echo  bool
	attrs []slog.Attr
}

// Enabled reports that every level is handled: DEBUG lines reach the file
// unconditionally because the file is the transport to the backend.
func (h *selfHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle converts the slog record into a LogRecord and writes both views.
func (h *selfHandler) Handle(_ context.Context, r slog.Record) error {
	rec := LogRecord{
		TS:     r.Time.UTC().Format(RFC3339Milli),
		Level:  levelFromSlog(r.Level),
		Source: SourceSelf,
		Msg:    r.Message,
	}
	component := ""
	absorb := func(a slog.Attr) bool {
		if a.Equal(slog.Attr{}) {
			return true
		}
		if a.Key == componentKey {
			if s, ok := a.Value.Any().(string); ok {
				component = s
			}
			return true
		}
		if rec.Fields == nil {
			rec.Fields = make(map[string]string)
		}
		rec.Fields[a.Key] = a.Value.String()
		return true
	}
	for _, a := range h.attrs {
		absorb(a)
	}
	r.Attrs(absorb)
	if component == "" {
		component = "misc"
	}
	rec.Component = component

	if line, err := rec.Encode(); err == nil && h.file != nil {
		logMu.Lock()
		_, _ = h.file.Write(append(line, '\n'))
		logMu.Unlock()
	}
	if h.echo {
		_, _ = fmt.Fprintln(os.Stderr, rec.textLine())
	}
	return nil
}

// WithAttrs returns a copy of the handler carrying the given attributes on
// every record it handles; this is the correlation path for slog.With.
func (h *selfHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := *h
	cp.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &cp
}

// WithGroup returns the handler unchanged; self logs do not use groups.
func (h *selfHandler) WithGroup(string) slog.Handler { return h }

// handlerFor picks the echo or file-only handler instance. Before
// InitFileLog has run (or when the log file could not be opened) the file is
// nil and the handler degrades to stderr only.
func handlerFor(echo bool) slog.Handler {
	if echo {
		if selfEchoHandler != nil {
			return selfEchoHandler
		}
		return &selfHandler{echo: true}
	}
	if selfFileHandler != nil {
		return selfFileHandler
	}
	return &selfHandler{}
}

// levelFromSlog maps a slog level onto the canonical LogRecord spelling.
func levelFromSlog(l slog.Level) LogLevel {
	switch {
	case l >= slog.LevelError:
		return LevelError
	case l >= slog.LevelWarn:
		return LevelWarning
	case l >= slog.LevelInfo:
		return LevelInfo
	default:
		return LevelDebug
	}
}

// slogLevel maps the public LogLevel onto slog levels for record creation.
func slogLevel(l LogLevel) slog.Level {
	switch normalizeLevel(l) {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarning:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// emitSelf routes one self message through the slog handler. echo controls
// the stderr mirror: daemon-level activity keeps it, short-lived CLI
// operations stay quiet on the terminal.
func emitSelf(level LogLevel, component string, echo bool, format string, args ...any) {
	InitFileLog()

	msg := fmt.Sprintf(format, args...)
	r := slog.NewRecord(time.Now(), slogLevel(level), strings.ToValidUTF8(msg, "\uFFFD"), 0)
	r.Add(slog.String(componentKey, component))
	_ = handlerFor(echo).Handle(context.Background(), r)
}

// writeLevel writes a self-log line to the log file and mirrors it to stderr.
func writeLevel(level LogLevel, component, format string, args ...any) {
	emitSelf(level, component, true, format, args...)
}

// writeLevelFile writes a self-log line to the log file without echoing it
// to stderr. This is the transport path for short-lived CLI operations whose
// logs must reach the backend but must not clutter the user's terminal.
func writeLevelFile(level LogLevel, component, format string, args ...any) {
	emitSelf(level, component, false, format, args...)
}

// Info logs a level-INFO self message with the given component.
func Info(component, format string, args ...any) {
	writeLevel(LevelInfo, component, format, args...)
}

// Warning logs a level-WARN self message with the given component.
func Warning(component, format string, args ...any) {
	writeLevel(LevelWarning, component, format, args...)
}

// Error logs a level-ERROR self message with the given component.
func Error(component, format string, args ...any) {
	writeLevel(LevelError, component, format, args...)
}

// Debug logs a level-DEBUG self message with the given component. DEBUG
// messages are written to the log file unconditionally; the file is the
// transport to the backend, not the terminal.
func Debug(component, format string, args ...any) {
	writeLevel(LevelDebug, component, format, args...)
}

// InfoFile logs a level-INFO self message to the log file only (no stderr
// echo). See writeLevelFile.
func InfoFile(component, format string, args ...any) {
	writeLevelFile(LevelInfo, component, format, args...)
}

// WarningFile logs a level-WARN self message to the log file only (no stderr
// echo). See writeLevelFile.
func WarningFile(component, format string, args ...any) {
	writeLevelFile(LevelWarning, component, format, args...)
}

// ErrorFile logs a level-ERROR self message to the log file only (no stderr
// echo). See writeLevelFile.
func ErrorFile(component, format string, args ...any) {
	writeLevelFile(LevelError, component, format, args...)
}

// DebugFile logs a level-DEBUG self message to the log file only (no stderr
// echo). See writeLevelFile.
func DebugFile(component, format string, args ...any) {
	writeLevelFile(LevelDebug, component, format, args...)
}
