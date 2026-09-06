package cmd

import (
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// GetAppInfo retrieves application information by ID or AppName
func GetAppInfo(identifier string) (*app.AppInfo, error) {
	manager := app.Manager.(*app.AppManager)
	if appInfo, exists := manager.Apps[identifier]; exists {
		return appInfo, nil
	}
	for _, a := range manager.Apps {
		if a.Name == identifier {
			return a, nil
		}
	}
	return nil, phelixerr.Newf(
		phelixerr.CodeNotFound,
		"application not found with ID or Name: %s",
		identifier,
	)
}

// DetermineAppParameters determines the name and port to use for an application
func DetermineAppParameters(appInfo *app.AppInfo, cmd *cobra.Command, defaultPort int) (string, int) {
	name := appInfo.ID
	if appInfo.Name != "" {
		name = appInfo.Name
	}

	usePort := defaultPort
	if !cmd.Flags().Changed("port") && appInfo.Port != 0 {
		usePort = appInfo.Port
	}

	return name, usePort
}

// currentDirOrError returns the working directory, or "" on failure. Callers
// treat "" as "no project config".
func currentDirOrError() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

// FormatStatus formats the application status with color
func FormatStatus(status string) string {
	switch status {
	case "running":
		return color.GreenString("running")
	case "degraded":
		return color.YellowString("degraded")
	case "stopped":
		return color.RedString("stopped")
	default:
		return status
	}
}
