package logs

import (
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"io"
	"os"
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
	mu      sync.Mutex
	dst     io.Writer
	stream  LogStream
	level   LogLevel
	buf     []byte
	discard bool
	err     error
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

// File exposes the underlying *os.File when dst is one. Process starters use
// it to attach the child's stdout/stderr directly to the log file descriptor:
// an *os.File passes through exec.Cmd without an intermediate OS pipe, so the
// child keeps a valid sink even after the parent process exits (a pipe would
// SIGPIPE-kill the app on its first write after the parent dies).
func (w *AppLogWriter) File() *os.File {
	if f, ok := w.dst.(*os.File); ok {
		return f
	}
	return nil
}

// Write buffers the incoming bytes and emits complete lines. Partial lines
// are held until terminated; trailing data is flushed by Close.
func (w *AppLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	remaining := p
	for len(remaining) > 0 {
		i := strings.IndexByte(string(remaining), '\n')
		end := i
		if end < 0 {
			end = len(remaining)
		}
		if len(w.buf)+end > 1<<20 {
			w.buf = nil
			w.discard = true
		}
		if !w.discard {
			w.buf = append(w.buf, remaining[:end]...)
		}
		if i < 0 {
			break
		}
		if w.discard {
			w.emitLocked("[line omitted: exceeds log size limit]")
		} else {
			w.emitLocked(string(w.buf))
		}
		w.buf = w.buf[:0]
		w.discard = false
		remaining = remaining[i+1:]
	}
	return len(p), w.err
}

// Close emits any buffered partial line and returns.
func (w *AppLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.discard {
		w.emitLocked("[line omitted: exceeds log size limit]")
		w.discard = false
	}
	if len(w.buf) > 0 {
		w.emitLocked(string(w.buf))
		w.buf = w.buf[:0]
	}
	return w.err
}

// emitLocked writes one prefixed line to the destination writer. Callers must
// hold w.mu.
func (w *AppLogWriter) emitLocked(line string) {
	msg := strings.TrimSuffix(strings.ReplaceAll(phelixerr.Redact(line), "\r", ""), "\n")

	ts := time.Now().Format(timeLayout)
	_, w.err = io.WriteString(w.dst, ts+" ["+string(w.level)+"] ["+string(w.stream)+"] "+msg+"\n")
}
