package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/env"
	"github.com/shirou/gopsutil/process"
)

// verifyProcessStatus checks if a process is running. It does NOT modify app
// state — callers that need to persist a status change must do so explicitly.
func (m *AppManager) verifyProcessStatus(app *AppInfo) bool {
	if app.Status != "running" || app.PID <= 0 {
		return false
	}

	p, err := process.NewProcess(int32(app.PID))
	if err != nil {
		return false
	}
	running, _ := p.IsRunning()
	return running
}

// startApplicationProcess starts the application process and updates the app info
func (m *AppManager) startApplicationProcess(id string, name string, port int, logFile string) error {
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %v", err)
	}
	defer f.Close()

	app, exists := m.Apps[id]
	if !exists {
		return fmt.Errorf("application %s not found", id)
	}

	if app.Directory == "" {
		return fmt.Errorf("application directory not found for ID %s", id)
	}

	binaryPath := filepath.Join(app.Directory, fmt.Sprintf("app_%s", id))
	cmd := exec.Command(binaryPath)
	cmd.Dir = app.Directory
	cmd.Stdout = f
	cmd.Stderr = f

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
		return fmt.Errorf("start failed: %v", err)
	}

	app.Cmd = cmd
	app.PID = cmd.Process.Pid
	app.Status = "running"
	app.Start = time.Now()
	app.Port = port
	app.LogFile = logFile
	app.BuildStatus = "built"
	app.UpdatedAt = time.Now()

	return nil
}

// stopApplicationProcess stops the application process gracefully, falling back to force kill if necessary
func (m *AppManager) stopApplicationProcess(app *AppInfo) error {
	if app.PID <= 0 {
		return nil
	}

	if app.Cmd != nil && app.Cmd.Process != nil {
		// In-process handle available — use it directly.
		if stdin, _ := app.Cmd.StdinPipe(); stdin != nil {
			stdin.Close()
		}

		if err := app.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
			fmt.Println("Failed to send SIGTERM:", err)
		}

		done := make(chan error, 1)
		go func() {
			done <- app.Cmd.Wait()
		}()

		select {
		case <-time.After(5 * time.Second):
			fmt.Println("Process did not stop gracefully, attempting SIGKILL")
			_ = app.Cmd.Process.Kill()
			<-done // reap the child so it doesn't linger as zombie
		case err := <-done:
			if err != nil {
				fmt.Println("Process exited with error:", err)
			}
		}
	} else {
		// app.Cmd is nil (loaded from persisted state) — operate by PID.
		proc, err := os.FindProcess(app.PID)
		if err != nil {
			return fmt.Errorf("failed to find process %d: %v", app.PID, err)
		}

		// Send SIGTERM and poll until the process exits or 5 s elapses.
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			fmt.Println("Failed to send SIGTERM:", err)
		}

		terminated := m.waitForProcessExit(app.PID, 5*time.Second)
		if !terminated {
			fmt.Println("Process did not stop gracefully, attempting SIGKILL")
			_ = proc.Kill()
			// Give SIGKILL a moment to take effect.
			time.Sleep(500 * time.Millisecond)
		}

		// Reap the child process to avoid zombie entry.
		syscall.Wait4(app.PID, nil, 0, nil)
	}

	// Final verification with gopsutil.
	time.Sleep(500 * time.Millisecond)
	p, err := process.NewProcess(int32(app.PID))
	if err == nil {
		alive, _ := p.IsRunning()
		if alive {
			fmt.Println("Process is still running, using system kill command")
			if killErr := exec.Command("kill", "-9", fmt.Sprintf("%d", app.PID)).Run(); killErr != nil {
				fmt.Printf("  kill -9 failed: %v\n", killErr)
			}
			time.Sleep(1 * time.Second)
		}
	}

	// One last check.
	p, err = process.NewProcess(int32(app.PID))
	if err == nil {
		alive, _ := p.IsRunning()
		if alive {
			return fmt.Errorf("failed to stop application '%s' (ID: %s): process still running", app.Name, app.ID)
		}
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
	if err != nil || !running {
		return 0, 0, fmt.Errorf("process is not running")
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

// isProcessRunning checks if a process is running via gopsutil.
func (m *AppManager) isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}

	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	running, _ := p.IsRunning()
	return running
}

// ensureLogDirectory ensures the log directory exists
func (m *AppManager) ensureLogDirectory() error {
	return os.MkdirAll(logDir, 0755)
}
