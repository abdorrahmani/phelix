package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/logs"
)

// TestChildSurvivesParentExitAndKeepsLogging reproduces the `phelix log`
// empty-output bug: a CLI process starts an app and exits immediately. The
// child must survive, keep its stdout attached to the log file (a real fd, not
// a pipe owned by the parent), and every line it writes must land in the file.
func TestChildSurvivesParentExitAndKeepsLogging(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}

	dir := t.TempDir()
	logFile := filepath.Join(dir, "app.log")

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", "-c", "echo first; sleep 1; echo second; sleep 1; echo third")
	stdoutW := logs.NewAppLogWriter(f, logs.StreamStdout)
	cmd.Stdout = stdoutW.File()
	cmd.Stderr = stdoutW.File()
	cmd.SysProcAttr = detachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid

	defer func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	// Simulate the short-lived CLI: close everything and let this test's
	// goroutine that started the child keep running (the child is in its own
	// session, so it is not killed with us).
	f.Close()

	// The child must still be alive after the parent "moved on".
	time.Sleep(300 * time.Millisecond)

	// Wait for the final line; the child keeps writing into the log file even
	// though the parent no longer holds any pipe.
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatal(err)
		}
		if containsLine(data, "third") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child output incomplete after parent kept running: %q\n(child pid %d alive=%v)",
				string(data), pid, isAlive(pid))
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !isAlive(pid) {
		t.Fatalf("child died before finishing its work (SIGPIPE-style kill?)")
	}
}

func containsLine(data []byte, want string) bool {
	return len(data) >= len(want) && string(data)[len(data)-len(want)-1:] == "\n"+want ||
		contains(data, []byte("\n"+want+"\n")) || beginsWithLine(data, want)
}

func beginsWithLine(data []byte, want string) bool {
	s := string(data)
	return len(s) >= len(want) && s[:len(want)] == want
}

func contains(data, sub []byte) bool {
	for i := 0; i+len(sub) <= len(data); i++ {
		match := true
		for j := range sub {
			if data[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func isAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
