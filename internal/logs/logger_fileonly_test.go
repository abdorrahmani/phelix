package logs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file-only loggers (InfoFile/WarningFile/ErrorFile/DebugFile) must write
// the structured self-log line to ~/.phelix/logs/phelix.log — the transport
// to the backend — but must NOT echo it to stderr. This is what keeps short-
// lived CLI operations (gRPC event reporting, version sync) from polluting
// the user's terminal. The regular Info/Warning/Error/Debug keep the stderr
// mirror so daemon activity stays observable.
func TestInfoFile_WritesFileNotStderr(t *testing.T) {
	// Repoint the logger's output into a temp HOME. Reset package state so
	// initFileLogLocked re-opens the (now temp) phelix.log.
	prevHandle := logFileHandle
	logFileHandle = nil
	defer func() { logFileHandle = prevHandle }()

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Pin the data dir into the temp HOME so path resolution is independent of
	// whether the test host is a Docker container (dataDir() would otherwise
	// pick /var/lib/phelix).
	t.Setenv("PHELIX_DATA_DIR", filepath.Join(home, ".phelix"))

	// Swap the process's real stderr so we can assert nothing leaks to it.
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

	// The write is synchronous; close the write end so ReadAll returns.
	_ = w.Close()

	stderrBytes, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	if len(stderrBytes) != 0 {
		t.Fatalf("InfoFile leaked to stderr: %q", string(stderrBytes))
	}

	// The message must be in the log file in the structured format.
	logPath := filepath.Join(home, ".phelix", "logs", "phelix.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read phelix.log: %v", err)
	}
	if !strings.Contains(string(data), "[INFO] [grpc] file-only message: action=start") {
		t.Fatalf("file-only line missing from phelix.log: %q", string(data))
	}
}

// The regular Info must keep writing to BOTH the file and stderr — daemon
// activity under a terminal/systemd stays observable.
func TestInfo_WritesFileAndStderr(t *testing.T) {
	prevHandle := logFileHandle
	logFileHandle = nil
	defer func() { logFileHandle = prevHandle }()

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
	if !strings.Contains(string(data), "[INFO] [daemon] observable message") {
		t.Fatalf("Info line missing from phelix.log: %q", string(data))
	}
}
