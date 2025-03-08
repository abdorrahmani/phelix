package app

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

type AppManagerInterface interface {
	GenerateAppID() string
	StartApplication(id string)
	StopApplication(id string)
	RestartApplication(id string) error
	StatusApplication(id string) (string, error)
	ListApplications() []struct {
		ID     string
		Status string
		PID    int
		Uptime string
	}
}

type AppInfo struct {
	ID     string
	Cmd    *exec.Cmd
	PID    int
	Status string
	Start  time.Time
}

type AppManager struct {
	Apps map[string]*AppInfo
	Lock sync.Mutex
}

var Manager AppManagerInterface = &AppManager{
	Apps: make(map[string]*AppInfo),
}

// GenerateAppID generates a unique ID for an application.
func (m *AppManager) GenerateAppID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// StartApplication starts an application by its ID.
func (m *AppManager) StartApplication(id string) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if app, exists := m.Apps[id]; exists && app.Status == "running" {
		fmt.Printf("Application %s is already running\n", id)
		return
	}
	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		fmt.Println("Start failed:", err)
		return
	}
	m.Apps[id] = &AppInfo{
		ID:     id,
		Cmd:    cmd,
		PID:    cmd.Process.Pid,
		Status: "running",
		Start:  time.Now(),
	}
	fmt.Printf("Application %s started successfully\n", id)
}

// StopApplication stops an application by its ID.
func (m *AppManager) StopApplication(id string) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if app, exists := m.Apps[id]; exists && app.Status == "running" {
		if err := app.Cmd.Process.Kill(); err != nil {
			fmt.Println("Failed to stop application:", err)
			return
		}
		app.Status = "stopped"
	} else {
		fmt.Printf("Application %s not found or not running\n", id)
	}
}

// RestartApplication restarts an application by its ID.
func (m *AppManager) RestartApplication(id string) error {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if app, exists := m.Apps[id]; exists && app.Status == "running" {
		if err := app.Cmd.Process.Kill(); err != nil {
			return fmt.Errorf("failed to stop application %s: %v", id, err)
		}
		app.Status = "stopped"
	} else {
		fmt.Printf("Application %s was not running, starting it now\n", id)
	}

	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to restart application %s: %v", id, err)
	}

	m.Apps[id] = &AppInfo{
		ID:     id,
		Cmd:    cmd,
		PID:    cmd.Process.Pid,
		Status: "running",
		Start:  time.Now(),
	}
	return nil
}

// StatusApplication returns the status of an application by its ID.
func (m *AppManager) StatusApplication(id string) (string, error) {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	if app, exists := m.Apps[id]; exists {
		if app.Cmd.ProcessState != nil && app.Cmd.ProcessState.Exited() {
			app.Status = "stopped"
		}
		return app.Status, nil
	}
	return "", errors.New("application not found")
}

// ListApplications returns a list of all applications with their details.
func (m *AppManager) ListApplications() []struct {
	ID     string
	Status string
	PID    int
	Uptime string
} {
	m.Lock.Lock()
	defer m.Lock.Unlock()

	var appList []struct {
		ID     string
		Status string
		PID    int
		Uptime string
	}

	for id, app := range m.Apps {
		if app.Cmd.ProcessState != nil && app.Cmd.ProcessState.Exited() {
			app.Status = "stopped"
		}

		var uptime string
		if app.Status == "running" {
			duration := time.Since(app.Start)
			uptime = formatDuration(duration)
		} else {
			uptime = "N/A"
		}

		appList = append(appList, struct {
			ID     string
			Status string
			PID    int
			Uptime string
		}{
			ID:     id,
			Status: app.Status,
			PID:    app.PID,
			Uptime: uptime,
		})
	}
	return appList
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	return fmt.Sprintf("%02dh %02dm %02ds", h, m, s)
}
