package logs

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file-only loggers (InfoFile/WarningFile/ErrorFile/DebugFile) must write
// the JSON self-log record to ~/.phelix/logs/phelix.log, the transport to the
// backend, but must NOT echo it to stderr. This is what keeps short-lived CLI
// operations (gRPC event reporting, version sync) from polluting the user's
// terminal. The regular Info/Warning/Error/Debug keep the stderr mirror so
// daemon activity stays observable.
func TestInfoFile_WritesFileNotStderr(t *testing.T) {
	prevHandle := logFileHandle
	prevEcho := selfEchoHandler
	prevFile := selfFileHandler
	logFileHandle = nil
	selfEchoHandler = nil
	selfFileHandler = nil
	defer func() {
		logFileHandle = prevHandle
		selfEchoHandler = prevEcho
		selfFileHandler = prevFile
	}()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(home, ".phelix"))

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	defer func() {
		os.Stderr = oldStderr
		_ = r.Close()
	}()

	InfoFile("grpc", "file-only message: action=%s", "start")

	_ = w.Close()

	stderrBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	if len(stderrBytes) != 0 {
		t.Fatalf("InfoFile leaked to stderr: %q", string(stderrBytes))
	}

	logPath := filepath.Join(home, ".phelix", "logs", "phelix.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read phelix.log: %v", err)
	}
	var rec LogRecord
	line := strings.TrimSpace(string(data))
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("phelix.log line is not a JSON record: %v (%q)", err, line)
	}
	if rec.Component != "grpc" || rec.Msg != "file-only message: action=start" || rec.Level != LevelInfo {
		t.Errorf("unexpected record: %+v", rec)
	}
	if rec.Source != SourceSelf {
		t.Errorf("self record source = %q, want self", rec.Source)
	}
}

// The regular Info must keep writing to BOTH the file (as JSON) and stderr
// (as the legacy human-readable line): daemon activity under a
// terminal/systemd stays observable.
func TestInfo_WritesFileAndStderr(t *testing.T) {
	prevHandle := logFileHandle
	prevEcho := selfEchoHandler
	prevFile := selfFileHandler
	logFileHandle = nil
	selfEchoHandler = nil
	selfFileHandler = nil
	defer func() {
		logFileHandle = prevHandle
		selfEchoHandler = prevEcho
		selfFileHandler = prevFile
	}()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(home, ".phelix"))

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	Info("daemon", "observable message")

	_ = w.Close()
	stderrBytes, _ := io.ReadAll(r)
	_ = r.Close()

	if !strings.Contains(string(stderrBytes), "[INFO] [daemon] observable message") {
		t.Fatalf("Info did not mirror to stderr: %q", string(stderrBytes))
	}

	logPath := filepath.Join(home, ".phelix", "logs", "phelix.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read phelix.log: %v", err)
	}
	var rec LogRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("phelix.log line is not a JSON record: %v (%q)", err, string(data))
	}
	if rec.Component != "daemon" || rec.Msg != "observable message" {
		t.Errorf("unexpected record: %+v", rec)
	}
}
