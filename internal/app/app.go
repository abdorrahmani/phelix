package app

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

type AppInfo struct {
	ID     string
	Cmd    *exec.Cmd
	PID    int
	Status string
	Start  time.Time
}

var (
	apps     = make(map[string]*AppInfo)
	appsLock sync.Mutex
)

func GenerateAppID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func StartApplication(id string) {
	appsLock.Lock()
	defer appsLock.Unlock()

	if app, exists := apps[id]; exists && app.Status == "running" {
		fmt.Printf("Application %s is already running\n", id)
		return
	}
	cmd := exec.Command(fmt.Sprintf("./app_%s", id))
	if err := cmd.Start(); err != nil {
		fmt.Println("Start failed:", err)
		return
	}
	apps[id] = &AppInfo{
		ID:     id,
		Cmd:    cmd,
		PID:    cmd.Process.Pid,
		Status: "running",
		Start:  time.Now(),
	}
	fmt.Printf("Application %s started successfully\n", id)
}

func StopApplication(id string) {
	appsLock.Lock()
	defer appsLock.Unlock()

	if app, exists := apps[id]; exists && app.Status == "running" {
		if err := app.Cmd.Process.Kill(); err != nil {
			fmt.Println("Failed to stop application:", err)
			return
		}
		app.Status = "stopped"
	} else {
		fmt.Printf("Application %s not found or not running\n", id)
	}
}

func RestartApplication(id string) error {
	appsLock.Lock()
	defer appsLock.Unlock()

	if app, exists := apps[id]; exists && app.Status == "running" {
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

	apps[id] = &AppInfo{
		ID:     id,
		Cmd:    cmd,
		PID:    cmd.Process.Pid,
		Status: "running",
		Start:  time.Now(),
	}
	return nil
}

func StatusApplication(id string) (string, error) {
	appsLock.Lock()
	defer appsLock.Unlock()

	if app, exists := apps[id]; exists {
		if app.Cmd.ProcessState != nil && app.Cmd.ProcessState.Exited() {
			app.Status = "stopped"
		}
		return app.Status, nil
	}
	return "", errors.New("application not found")
}

func ListApplications() []struct {
	ID     string
	Status string
	PID    int
	Uptime string
} {
	appsLock.Lock()
	defer appsLock.Unlock()

	var appList []struct {
		ID     string
		Status string
		PID    int
		Uptime string
	}

	for id, app := range apps {
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
