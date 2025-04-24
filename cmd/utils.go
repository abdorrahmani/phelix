package cmd

import (
	"fmt"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// GetAppInfo retrieves application information by ID
func GetAppInfo(id string) (*app.AppInfo, error) {
	appInfo, exists := app.Manager.(*app.AppManager).Apps[id]
	if !exists {
		return nil, fmt.Errorf("application with ID %s not found", id)
	}
	return appInfo, nil
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

// FormatStatus formats the application status with color
func FormatStatus(status string) string {
	switch status {
	case "running":
		return color.GreenString("running")
	case "stopped":
		return color.RedString("stopped")
	default:
		return status
	}
}
