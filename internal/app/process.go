package app

import (
	"context"
	"errors"
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
	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/resources"
	"github.com/abdorrahmani/phelix/internal/secrets"
	"github.com/shirou/gopsutil/process"
)

// mergeEnvByKey keeps the last value of each name, matching process launch behavior.
func mergeEnvByKey(base, overlay []string) []string {
	return secrets.MergeEnv(base, overlay)
}

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

	return m.appProcessRunning(app)
}

// startApplicationProcess starts the application process and updates the app info
func (m *AppManager) startApplicationProcess(id string, name string, portNum int, logFile string) error {
	return m.startApplicationProcessWithSecrets(id, name, portNum, logFile, nil)
}

func (m *AppManager) startApplicationProcessWithSecrets(id string, name string, portNum int, logFile string, pinned *secrets.PinnedSet) error {

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

	stdoutW := logs.NewAppLogWriter(f, logs.StreamStdout)
	stderrW := logs.NewAppLogWriter(f, logs.StreamStderr)

	cmd.Stdout = stdoutW.File()
	cmd.Stderr = stderrW.File()

	cmd.SysProcAttr = detachedSysProcAttr()

	envVars := os.Environ() // Start with current environment
	appEnvVars, err := env.Instance.GetAllEnvVars(id)

	if err != nil {
		f.Close()
		return phelixerr.Wrapf(phelixerr.CodeEncryption, err,
			"failed to load environment variables for application %q (ID: %s)", name, id)
	}

	for key, value := range appEnvVars {
		envVars = append(envVars, fmt.Sprintf("%s=%s", key, value))
	}

	var secretEnvs []string
	if pinned != nil {
		secretEnvs = pinned.Overlay()
	} else {
		secretEnvs, err = secrets.ClassicEnvOverlay(context.Background(), id)
	}
	if err != nil {
		f.Close()
		return phelixerr.Wrapf(phelixerr.CodeOf(err), err,
			"failed to resolve secrets for application %q (ID: %s)", name, id)
	}
	envVars = mergeEnvByKey(envVars, secretEnvs)

	envVars = append(envVars, fmt.Sprintf("PORT=%d", portNum))
	cmd.Env = envVars

	closeDrain, err := logs.AttachDrain(cmd, f)
	if err != nil {
		f.Close()
		return err
	}
	defer closeDrain()
	releaseSecrets := phelixerr.AcquireSecretValues(phelixerr.SecretValuesInEnv(cmd.Env))
	inst, err := resources.Start(cmd, app.Resources)
	if err != nil {
		releaseSecrets()
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

	app.SecretEnvs = phelixerr.SecretEnvNames(cmd.Env)
	app.releaseSecrets = releaseSecrets
	app.Cmd = cmd
	app.PID = cmd.Process.Pid
	app.StartTicks = 0
	app.Status = "running"
	app.Start = time.Now()
	app.Port = portNum
	app.LogFile = logFile
	app.BuildStatus = "built"
	app.UpdatedAt = time.Now()

	app.AutoStart = true
	app.logFileHandle = f
	identity, err := processex.Identity(app.PID)
	if err != nil {
		return errors.Join(phelixerr.New(phelixerr.CodeProcessFailed, "cannot identify started application"), m.stopNewlyStarted(app))
	}
	app.StartTicks = identity.StartTime

	addr := fmt.Sprintf("127.0.0.1:%d", portNum)
	deadline := time.Now().Add(startupListenTimeout)
	listening := false
	for time.Now().Before(deadline) {
		if port.IsListening(addr) {
			listening = true
			break
		}
		if !m.appProcessRunning(app) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !listening {
		if !m.appProcessRunning(app) {
			app.Status = "failed"
			app.PID = 0

			oomErr := app.classifyResourceExit()
			m.waitAndCloseLog(app)
			if oomErr != nil {
				return oomErr
			}
			return phelixerr.Newf(
				phelixerr.CodeProcessFailed,
				"application %q (ID: %s) exited immediately after start; see log %s",
				name, id, logFile,
			)
		}

		if err := m.stopNewlyStarted(app); err != nil {
			return err
		}

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

// stopNewlyStarted retains ownership if a failed launch cannot be stopped.
func (m *AppManager) stopNewlyStarted(app *AppInfo) error {
	if err := m.stopApplicationProcess(app); err != nil {
		return err
	}
	app.Status, app.PID = "failed", 0
	return nil
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
		if m.isProcessRunning(app.Cmd.Process.Pid) {
			return
		}
		_ = app.Cmd.Wait()
	}

	app.closeResourceInstance()
	if app.releaseSecrets != nil {
		app.releaseSecrets()
		app.releaseSecrets = nil
	}
	if app.logFileHandle != nil {
		app.logFileHandle.Close()
		app.logFileHandle = nil
	}
}

// stopApplicationProcess stops a verified process before releasing its resources.
func (m *AppManager) stopApplicationProcess(app *AppInfo) error {
	if app.PID <= 0 || !m.appProcessRunning(app) {
		m.waitAndCloseLog(app)
		return nil
	}
	proc, err := os.FindProcess(app.PID)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "cannot find application process", err)
	}
	defer proc.Release()
	if err := proc.Signal(syscall.SIGTERM); err != nil && m.appProcessRunning(app) {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "cannot stop application process", err)
	}
	if !m.waitForProcessExit(app, 5*time.Second) {
		if err := proc.Kill(); err != nil && m.appProcessRunning(app) {
			return phelixerr.Wrap(phelixerr.CodeProcessFailed, "cannot kill application process", err)
		}
		if !m.waitForProcessExit(app, 5*time.Second) {
			return phelixerr.New(phelixerr.CodeProcessFailed, "cannot confirm application process stopped")
		}
	}
	m.waitAndCloseLog(app)
	app.Cmd = nil
	return nil
}

// waitForProcessExit checks the same process identity until it exits or time runs out.
func (m *AppManager) waitForProcessExit(app *AppInfo, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for m.appProcessRunning(app) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// getProcessMetrics retrieves RAM and CPU usage for the given process PID.
func (m *AppManager) getProcessMetrics(pid int) (uint64, float64, error) {
	return ProcessUsage(pid)
}

// ProcessUsage retrieves RAM bytes and CPU percent for one live process. It is
// exported for monitoring consumers that track a process the manager does not
// own, such as a worker replica.
func ProcessUsage(pid int) (uint64, float64, error) {
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

	memInfo, err := p.MemoryInfo()
	if err == nil && memInfo != nil {
		ramUsage = memInfo.RSS
	}

	cpuPercent, err := p.CPUPercent()
	if err == nil {
		cpuUsage = cpuPercent
	}

	return ramUsage, cpuUsage, nil
}

// isProcessRunning checks live process state without wall-clock estimates.
func (m *AppManager) isProcessRunning(pid int) bool {
	return processex.Alive(pid)
}

// ensureLogDirectory ensures the log directory exists
func (m *AppManager) ensureLogDirectory() error {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create log directory %s", logDir)
	}
	return nil
}

// appProcessRunning rejects recycled process IDs before adoption or termination.
func (m *AppManager) appProcessRunning(info *AppInfo) bool {
	if !m.isProcessRunning(info.PID) {
		return false
	}
	if info.StartTicks != 0 {
		id, err := processex.Identity(info.PID)
		return err == nil && id.StartTime == info.StartTicks
	}
	return true
}
