package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/process"
)

// verifyProcessStatus checks if a process is running and updates its status
func (m *AppManager) verifyProcessStatus(app *AppInfo) bool {
	if app.Status != "running" || app.PID <= 0 {
		return false
	}

	// Try using gopsutil first
	if p, err := process.NewProcess(int32(app.PID)); err == nil {
		if running, _ := p.IsRunning(); running {
			// Additional verification using system commands
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", app.PID))
			} else {
				cmd = exec.Command("ps", "-p", fmt.Sprintf("%d", app.PID))
			}
			if err := cmd.Run(); err == nil {
				return true
			}
		}
	}

	// If we get here, the process is not running
	app.Status = "stopped"
	app.UpdatedAt = time.Now()
	m.SaveState()
	return false
}

func (m *AppManager) ensureLogDirectory() error {
	return os.MkdirAll(logDir, 0755)
}

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

func (m *AppManager) stopApplicationProcess(app *AppInfo) error {
	if app.Cmd != nil && app.Cmd.Process != nil {
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
			if err := app.Cmd.Process.Kill(); err != nil {
				return fmt.Errorf("failed to force kill process: %v", err)
			}
		case err := <-done:
			if err != nil {
				fmt.Println("Process exited with error:", err)
			}
		}
	}

	time.Sleep(2 * time.Second)
	p, err := process.NewProcess(int32(app.PID))
	if err == nil {
		alive, _ := p.IsRunning()
		if alive {
			fmt.Println("Process is still running, using system kill command")
			exec.Command("kill", "-9", fmt.Sprintf("%d", app.PID)).Run()
			time.Sleep(1 * time.Second)
		}
	}

	p, err = process.NewProcess(int32(app.PID))
	if err == nil {
		alive, _ := p.IsRunning()
		if alive {
			return fmt.Errorf("failed to stop application '%s' (ID: %s): process still running", app.Name, app.ID)
		}
	}

	return nil
}

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

// isProcessRunning checks if a process is running using multiple methods
func (m *AppManager) isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}

	// Try using gopsutil first
	if p, err := process.NewProcess(int32(pid)); err == nil {
		if running, _ := p.IsRunning(); running {
			// Additional verification using system commands
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid))
			} else {
				cmd = exec.Command("ps", "-p", fmt.Sprintf("%d", pid))
			}
			if err := cmd.Run(); err == nil {
				return true
			}
		}
	}

	return false
}
