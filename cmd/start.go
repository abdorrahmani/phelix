package cmd

import (
	"fmt"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var port int

var StartCmd = &cobra.Command{
	Use:   "start [ID] --port <PORT>",
	Short: "Starts a specific application by its ID or all applications if no ID is provided",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load state: %v", err)
		}

		// If no ID provided, start all apps
		if len(args) == 0 {
			apps := app.Manager.ListApplications()
			if len(apps) == 0 {
				fmt.Println("No applications found to start.")
				return nil
			}

			fmt.Println("Starting all applications...")
			for _, appInfo := range apps {
				if appInfo.Status != "running" {
					fmt.Printf("Starting application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
					if err := app.Manager.StartApplication(appInfo.ID, appInfo.Port, appInfo.Name); err != nil {
						fmt.Printf("Warning: Failed to start application '%s' (ID: %s): %v\n", appInfo.Name, appInfo.ID, err)
						continue
					}
					fmt.Printf("Application '%s' (ID: %s) started successfully\n", appInfo.Name, appInfo.ID)
				} else {
					fmt.Printf("Application '%s' (ID: %s) is already running\n", appInfo.Name, appInfo.ID)
				}
			}
			return nil
		}

		// Start specific app
		id := args[0]

		appInfo, err := GetAppInfo(id)
		if err != nil {
			return err
		}

		name, usePort := DetermineAppParameters(appInfo, cmd, port)
		fmt.Printf("Starting application '%s' (ID: %s) on port %d\n", name, id, usePort)

		if err := app.Manager.StartApplication(id, usePort, name); err != nil {
			return fmt.Errorf("failed to start application '%s' (ID: %s): %v", name, id, err)
		}

		fmt.Printf("Application '%s' (ID: %s) started successfully on port %d\n", name, id, usePort)
		return nil
	},
}

func init() {
	StartCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the application on")
}
