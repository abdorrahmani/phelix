package app

import (
	"os/exec"
	"time"
)

// AppManagerInterface defines the contract for application management
type AppManagerInterface interface {
	GenerateAppID() string
	StartApplication(id string, port int, name string) error
	StopApplication(id string) error
	RestartApplication(id string) error
	StatusApplication(id string) (AppStatus, error)
	ListApplications() []AppListItem
	SaveState() error
	LoadState() error
	RemoveApplication(id string) error
}

// AppInfo represents the state of a single application
type AppInfo struct {
	ID          string
	Name        string
	Cmd         *exec.Cmd
	PID         int
	Status      string
	Start       time.Time
	Port        int
	LogFile     string
	BuildStatus string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Directory   string // Directory where the application is located
	// Language represents the programming language of the project (go, rust, ...)
	Language string
	// NoUpload indicates whether this app should be uploaded/shared with the server
	NoUpload bool
}

// AppStatus represents the current status of an application
type AppStatus struct {
	ID          string
	Name        string
	Status      string
	PID         int
	Uptime      string
	RAMUsage    uint64
	CPUUsage    float64
	BuildStatus string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Language    string
}

// AppListItem represents a simplified view of an application for listing
type AppListItem struct {
	ID          string
	Name        string
	Status      string
	PID         int
	Port        int
	Uptime      string
	BuildStatus string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Language    string
}
