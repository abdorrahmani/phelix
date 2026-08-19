package logs

import (
	"io"
	"strings"
	"sync"
	"time"
)

// AppLogWriter captures a single byte stream (an app's stdout or stderr) and
// re-emits it one complete line at a time, prefixed with the standard
// structured log format. The app's raw output is written to the shared app
// log file as:
//
//	2006/01/02 15:04:05 [LEVEL] [stream] message
//
// stdout lines default to [INFO]; stderr lines default to [ERROR]. This gives
// every captured line an exact, authored level and marks which stream it came
// from — parseLogLine reads these markers back verbatim and the backend
// receives them as MonitorLogEntry.stream / .level.
//
// Lines are written under a shared mutex so stdout and stderr writes from two
// goroutines never interleave mid-line.
type AppLogWriter struct {
	mu     sync.Mutex
	dst    io.Writer
	stream LogStream
	level  LogLevel
	buf    []byte // partial line not yet terminated by '\n'
}

// NewAppLogWriter wraps dst (the app log file) for a single output stream.
// stdout→INFO, stderr→ERROR.
func NewAppLogWriter(dst io.Writer, stream LogStream) *AppLogWriter {
	def := LevelInfo
	if stream == StreamStderr {
		def = LevelError
	}
	return &AppLogWriter{
		dst:    dst,
		stream: stream,
		level:  def,
	}
}

// Write buffers the incoming bytes and emits complete lines. Partial lines
// are held until terminated; trailing data is flushed by Close.
func (w *AppLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	remaining := p
	for {
		i := strings.IndexByte(string(remaining), '\n')
		if i < 0 {
			w.buf = append(w.buf, remaining...)
			break
		}
		line := append(w.buf, remaining[:i]...)
		w.buf = w.buf[:0]
		w.emitLocked(string(line))
		remaining = remaining[i+1:]
	}
	return len(p), nil
}

// Close emits any buffered partial line and returns.
func (w *AppLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emitLocked(string(w.buf))
		w.buf = w.buf[:0]
	}
	return nil
}

// emitLocked writes one prefixed line to the destination writer. Callers must
// hold w.mu.
func (w *AppLogWriter) emitLocked(line string) {
	// Normalize stray carriage returns (e.g. \r\n progress bars) so each
	// physical line stays one log entry.
	msg := strings.TrimSuffix(strings.ReplaceAll(line, "\r", ""), "\n")

	ts := time.Now().Format(timeLayout)
	_, _ = io.WriteString(w.dst, ts+" ["+string(w.level)+"] ["+string(w.stream)+"] "+msg+"\n")
}
