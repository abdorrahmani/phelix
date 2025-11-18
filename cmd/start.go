package cmd

import (
	"fmt"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var port int

var StartCmd = &cobra.Command{
	Use:   "start <ID|AppName> --port <PORT>",
	Short: "Starts an application by ID or AppName. If no argument is provided, starts all applications.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("⚠ Failed to load state: %v", err)
		}

		// If no ID or AppName provided, start all apps
		if len(args) == 0 {
			apps := app.Manager.ListApplications()
			if len(apps) == 0 {
				fmt.Println("No applications found to start.")
				return nil
			}

			fmt.Println("Starting all applications...")
			for _, a := range apps {
				if a.Status == "running" {
					fmt.Printf("• '%s' (ID: %s) is already running\n", a.Name, a.ID)
					continue
				}

				fmt.Printf("• Starting '%s' (ID: %s) on port %d...\n", a.Name, a.ID, a.Port)
				if err := app.Manager.StartApplication(a.ID, a.Port, a.Name); err != nil {
					fmt.Printf("  ⚠ Failed to start '%s' (ID: %s): %v\n", a.Name, a.ID, err)
					continue
				}
				fmt.Printf("  ✓ '%s' started successfully\n", a.Name)
			}
			return nil
		}

		// Start specific app
		identifier := args[0]

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		name, usePort := DetermineAppParameters(appInfo, cmd, port)

		fmt.Printf("Starting application '%s' (ID: %s) on port %d\n", name, appInfo.ID, usePort)

		if err := app.Manager.StartApplication(appInfo.ID, usePort, name); err != nil {
			return fmt.Errorf("⚠ Failed to start application '%s' (ID: %s): %v", name, appInfo.ID, err)
		}

		fmt.Printf("✓ Application '%s' (ID: %s) started successfully on port %d\n", name, appInfo.ID, usePort)
		return nil
	},
}

func init() {
	StartCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the application on")
}
