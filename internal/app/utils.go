package app

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// calculateUptime calculates the uptime of an application
func (m *AppManager) calculateUptime(app *AppInfo) string {
	if app.Status != "running" {
		return "N/A"
	}
	duration := time.Since(app.Start)
	return FormatDuration(duration)
}

// verifyApplicationBinary checks if the application binary exists
func (m *AppManager) verifyApplicationBinary(id string) bool {
	binaryPath := fmt.Sprintf("./app_%s", id)
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		return false
	}
	return true
}

// FormatDuration formats a duration into a human-readable string
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	return fmt.Sprintf("%02dh %02dm %02ds", h, m, s)
}

// GetGophelApps returns a list of apps that start with "gophel"
func GetGophelApps() ([]string, error) {
	apps := Manager.ListApplications()
	var gophelApps []string

	for _, app := range apps {
		if strings.HasPrefix(app.Name, "gophel") {
			gophelApps = append(gophelApps, app.Name)
		}
	}

	return gophelApps, nil
}
