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
	"github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/resources"
	"github.com/shirou/gopsutil/process"
)

// startupListenTimeout bounds the wait for an application to bind its port
// after the process is spawned. Compiles are done by then; only process start
// + app init live inside this window.
const startupListenTimeout = 10 * time.Second

// verifyProcessStatus checks if a process is running. It does NOT modify app
// state — callers that need to persist a status change must do so explicitly.
func (m *AppManager) verifyProcessStatus(app *AppInfo) bool {
	if app.Status != "running" || app.PID <= 0 {
		return false
	}

	return m.isProcessRunning(app.PID)
}

// startApplicationProcess starts the application process and updates the app info
func (m *AppManager) startApplicationProcess(id string, name string, portNum int, logFile string) error {

	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to open log file %s", logFile)
	}

	app, exists := m.Apps[id]
	if !exists {
		f.Close()
		return phelixerr.Newf(phelixerr.CodeNotFound, "application %s not found", id)
	}

	if app.Directory == "" {
		f.Close()
		return phelixerr.Newf(phelixerr.CodeNotFound, "application directory not found for ID %s", id)
	}

	binaryPath := filepath.Join(app.Directory, fmt.Sprintf("app_%s", id))
	cmd := exec.Command(binaryPath)
	cmd.Dir = app.Directory
	// Capture stdout and stderr separately so each line in the app log file
	// carries an exact [stdout]/[stderr] marker and level (see AppLogWriter).
	stdoutW := logs.NewAppLogWriter(f, logs.StreamStdout)
	stderrW := logs.NewAppLogWriter(f, logs.StreamStderr)
	// Hand the child the raw file descriptor rather than an OS pipe: a pipe's
	// read end dies with this short-lived CLI process, so the app's next write
	// would get SIGPIPE and be killed — losing every later log line. Writing
	// straight into the shared log file needs no parent-side reader at all.
	cmd.Stdout = stdoutW.File()
	cmd.Stderr = stderrW.File()
	// Detach into its own session so the app survives this CLI invocation
	// exiting, plus terminal hangup / Ctrl+C on `phelix start`.
	cmd.SysProcAttr = detachedSysProcAttr()

	// Inject encrypted environment variables
	envVars := os.Environ() // Start with current environment
	appEnvVars, err := env.Instance.GetAllEnvVars(id)
	if err == nil {
		// If env vars exist, append them to the process environment
		for key, value := range appEnvVars {
			envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
		}
	}
	// Port contract: Phelix owns the runtime port. Override any inherited or
	// app-stored PORT so the managed application listens on the requested one.
	envVars = append(envVars, fmt.Sprintf("PORT=%d", portNum))
	cmd.Env = envVars

	inst, err := resources.Start(cmd, app.Resources)
	if err != nil {
		f.Close()
		return phelixerr.Wrapf(
			phelixerr.CodeProcessFailed,
			err,
			"failed to start application %q (ID: %s)",
			name, id,
		)
	}
	if inst != nil {
		app.resourceInstance = inst
	}

	app.Cmd = cmd
	app.PID = cmd.Process.Pid
	app.Status = "running"
	app.Start = time.Now()
	app.Port = portNum
	app.LogFile = logFile
	app.BuildStatus = "built"
	app.UpdatedAt = time.Now()
	// The app was intentionally started; it should be restored the next time
	// the monitor daemon launches (e.g. after a machine reboot).
	app.AutoStart = true
	app.logFileHandle = f

	// Runtime port verification: the process existing is not success. Poll
	// until the application accepts connections on its port OR dies. A process
	// that stays alive but never binds usually means a hardcoded port.
	addr := fmt.Sprintf("127.0.0.1:%d", portNum)
	deadline := time.Now().Add(startupListenTimeout)
	listening := false
	for time.Now().Before(deadline) {
		if port.IsListening(addr) {
			listening = true
			break
		}
		if !m.isProcessRunning(app.PID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !listening {
		if !m.isProcessRunning(app.PID) {
			app.Status = "failed"
			app.PID = 0
			// classify a cgroup memory OOM kill before the cleanup
			// lease is released, so a memory-limit death is not reported as a
			// generic crash. Every other exit keeps the existing error.
			if oomErr := app.classifyResourceExit(); oomErr != nil {
				return oomErr
			}
			return phelixerr.Newf(
				phelixerr.CodeProcessFailed,
				"application %q (ID: %s) exited immediately after start; see log %s",
				name, id, logFile,
			)
		}

		// Kill only the instance Phelix just started — never a pre-existing
		// one — so a failed validation cannot leak a stray process.
		m.stopNewlyStarted(app)

		return phelixerr.Newf(
			phelixerr.CodePortUnavailable,
			"application failed port validation\n\n"+
				"Phelix started the application with:\n\n    PORT=%d\n\n"+
				"but nothing is listening on:\n\n    :%d\n\n"+
				"The application may be using a hardcoded port.\n\n"+
				"Phelix expects applications to read the PORT environment variable.\n\n"+
				"Run:\n\n    phelix doctor\n\nto diagnose the project.",
			portNum, portNum,
		)
	}

	return nil
}

// stopNewlyStarted terminates the process this method just spawned after a
// failed startup validation. It touches only app.Cmd — the handle created by
// the surrounding startApplicationProcess call — never an instance from a
// previous invocation.
func (m *AppManager) stopNewlyStarted(app *AppInfo) {
	if app.Cmd == nil || app.Cmd.Process == nil {
		return
	}
	_ = app.Cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = app.Cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = app.Cmd.Process.Kill()
		<-done
	}
	app.Status = "failed"
	app.PID = 0
	app.closeResourceInstance()
}

// classifyResourceExit closes the app's resource tracking handle and reports
// the structured resource-OOM failure when the instance was killed by its
// cgroup memory limit (memory.events oom_kill increased during its
// lifetime). It returns nil for every other exit — including when the OOM
// evidence itself is unreadable — so existing lifecycle behavior is
// unchanged. The handle is consumed either way.
func (a *AppInfo) classifyResourceExit() error {
	inst := a.resourceInstance
	a.resourceInstance = nil
	if inst == nil {
		return nil
	}
	oom, err := inst.ResourceOOM()
	_ = inst.Close()
	if err != nil || !oom {
		return nil
	}
	limit := ""
	if a.Resources.Memory != "" {
		limit = " of " + a.Resources.Memory
	}
	return phelixerr.Newf(
		phelixerr.CodeResourceOOM,
		"application %q (ID: %s) exceeded its configured memory limit%s and was killed by the kernel OOM killer; see log %s",
		a.Name, a.ID, limit, a.LogFile,
	)
}

// closeResourceInstance releases the cgroup cleanup lease after an exit was
// observed without classifying it (intentional stop paths).
func (a *AppInfo) closeResourceInstance() {
	if a.resourceInstance != nil {
		_ = a.resourceInstance.Close()
		a.resourceInstance = nil
	}
}

// waitAndCloseLog reaps an exited child process and releases its log file
// handle. It exists so the monitor daemon does not leak one fd per app while
// apps keep their stdout/stderr attached to the log file for their whole
// lifetime.
func (m *AppManager) waitAndCloseLog(app *AppInfo) {
	if app.Cmd != nil && app.Cmd.Process != nil {
		_ = app.Cmd.Wait()
	}
	// The child was reaped; its cgroup evidence window is over, so release the
	// cleanup lease before the log handle.
	app.closeResourceInstance()
	if app.logFileHandle != nil {
		app.logFileHandle.Close()
		app.logFileHandle = nil
	}
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
			fErr := phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to send SIGTERM to application '%s' (ID: %s)",
				app.Name, app.ID,
			)
			m.waitAndCloseLog(app)
			return fErr
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

	// Reap the child (if we own it) and release the log file handle that was
	// kept open for the child's stdout/stderr.
	m.waitAndCloseLog(app)
	app.Cmd = nil

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
