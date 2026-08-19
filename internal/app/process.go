package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/env"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/shirou/gopsutil/process"
)

// verifyProcessStatus checks if a process is running. It does NOT modify app
// state — callers that need to persist a status change must do so explicitly.
func (m *AppManager) verifyProcessStatus(app *AppInfo) bool {
	if app.Status != "running" || app.PID <= 0 {
		return false
	}

	return m.isProcessRunning(app.PID)
}

// startApplicationProcess starts the application process and updates the app info
func (m *AppManager) startApplicationProcess(id string, name string, port int, logFile string) error {
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to open log file %s", logFile)
	}
	defer f.Close()

	app, exists := m.Apps[id]
	if !exists {
		return phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found", id)
	}

	if app.Directory == "" {
		return phelixerr.Newf(phelixerr.CodeNotFound, "application directory not found for ID %s", id)
	}

	binaryPath := filepath.Join(app.Directory, fmt.Sprintf("app_%s", id))
	cmd := exec.Command(binaryPath)
	cmd.Dir = app.Directory
	// Capture stdout and stderr separately so each line in the app log file
	// carries an exact [stdout]/[stderr] marker and level (see AppLogWriter).
	// Both writers share the same underlying file handle; the writer's own
	// mutex keeps lines from interleaving mid-line.
	cmd.Stdout = logs.NewAppLogWriter(f, logs.StreamStdout)
	cmd.Stderr = logs.NewAppLogWriter(f, logs.StreamStderr)

	// Inject encrypted environment variables
	envVars := os.Environ() // Start with current environment
	appEnvVars, err := env.Instance.GetAllEnvVars(id)
	if err == nil {
		// If env vars exist, append them to the process environment
		for key, value := range appEnvVars {
			envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
		}
	}
	cmd.Env = envVars

	if err := cmd.Start(); err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeProcessFailed,
			err,
			"failed to start application %q (ID: %s)",
			name, id,
		)
	}

	app.Cmd = cmd
	app.PID = cmd.Process.Pid
	app.Status = "running"
	app.Start = time.Now()
	app.Port = port
	app.LogFile = logFile
	app.BuildStatus = "built"
	app.UpdatedAt = time.Now()
	// The app was intentionally started; it should be restored the next time
	// the monitor daemon launches (e.g. after a machine reboot).
	app.AutoStart = true

	return nil
}

// stopApplicationProcess stops the application process gracefully, falling back to force kill if necessary
func (m *AppManager) stopApplicationProcess(app *AppInfo) error {
	if app.PID <= 0 {
		return nil
	}
	// A process that has already exited (including a zombie that has not yet
	// been reaped by its parent) no longer holds resources such as the app's
	// listening port. Treat it as stopped instead of trying to signal its stale
	// PID. gopsutil's IsRunning reports zombies as running because their /proc
	// entry still exists.
	if !m.isProcessRunning(app.PID) {
		return nil
	}

	if app.Cmd != nil && app.Cmd.Process != nil {
		// In-process handle available — use it directly.
		if stdin, _ := app.Cmd.StdinPipe(); stdin != nil {
			stdin.Close()
		}

		if err := app.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
			return phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to send SIGTERM to application '%s' (ID: %s)",
				app.Name, app.ID,
			)
		}

		done := make(chan error, 1)
		go func() {
			done <- app.Cmd.Wait()
		}()

		select {
		case <-time.After(5 * time.Second):
			// Graceful stop timed out — fall back to SIGKILL. This is a
			// recovery path, not a failure of the stop itself, so it does not
			// return an error; the SIGKILL outcome is verified below.
			_ = app.Cmd.Process.Kill()
			<-done // reap the child so it doesn't linger as zombie
		case <-done:
			// The process exited. Its exit status is conveyed by the
			// final waitForProcessExit check; a non-zero exit here is not
			// an error of the stop operation itself.
		}
	} else {
		// app.Cmd is nil (loaded from persisted state) — operate by PID.
		proc, err := os.FindProcess(app.PID)
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "failed to find process %d", app.PID)
		}

		// Send SIGTERM and poll until the process exits or 5 s elapses.
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to send SIGTERM to application '%s' (ID: %s)",
				app.Name, app.ID,
			)
		}

		terminated := m.waitForProcessExit(app.PID, 5*time.Second)
		if !terminated {
			// Graceful stop timed out — fall back to SIGKILL (recovery path;
			// the SIGKILL outcome is verified below).
			_ = proc.Kill()
			// Give SIGKILL a moment to take effect.
			time.Sleep(500 * time.Millisecond)
		}

	}

	// SIGKILL is asynchronous for a process managed by an earlier invocation,
	// so give the kernel a short, bounded interval to finish termination.
	if !m.waitForProcessExit(app.PID, time.Second) {
		return phelixerr.Newf(
			phelixerr.CodeProcessFailed,
			"failed to stop application '%s' (ID: %s): process still running",
			app.Name, app.ID,
		)
	}

	return nil
}

// waitForProcessExit polls isProcessRunning until the process exits or the
// timeout elapses. Returns true if the process exited within the timeout.
func (m *AppManager) waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.After(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			if !m.isProcessRunning(pid) {
				return true
			}
		}
	}
}

// getProcessMetrics retrieves RAM and CPU usage for the given process PID
func (m *AppManager) getProcessMetrics(pid int) (uint64, float64, error) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, 0, err
	}

	// Check if process is actually running
	running, err := p.IsRunning()
	if err != nil {
		return 0, 0, phelixerr.Wrap(phelixerr.CodeProcessFailed, "process is not running", err)
	}
	if !running {
		return 0, 0, phelixerr.New(phelixerr.CodeProcessFailed, "process is not running")
	}

	var ramUsage uint64
	var cpuUsage float64

	// Try to get memory info
	memInfo, err := p.MemoryInfo()
	if err == nil && memInfo != nil {
		ramUsage = memInfo.RSS
	}

	// Try to get CPU usage
	cpuPercent, err := p.CPUPercent()
	if err == nil {
		cpuUsage = cpuPercent
	}

	return ramUsage, cpuUsage, nil
}

// isProcessRunning checks whether a PID represents a live process. A zombie
// still has a PID and is reported as running by gopsutil, but has already
// exited and cannot keep an application port open.
func (m *AppManager) isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}

	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	running, err := p.IsRunning()
	if err != nil || !running {
		return false
	}

	status, err := p.Status()
	return err != nil || status != "Z"
}

// ensureLogDirectory ensures the log directory exists
func (m *AppManager) ensureLogDirectory() error {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create log directory %s", logDir)
	}
	return nil
}
