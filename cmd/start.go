package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/spf13/cobra"
)

var (
	port        int
	startEnsure bool
)

var StartCmd = &cobra.Command{
	Use:   "start <ID|AppName> --port <PORT>",
	Short: "Starts an application by ID or AppName. If no argument is provided, starts all applications.",
	Long:  "Starts an existing application. With --ensure, it becomes idempotent: build the app if it does not exist, start it if stopped, and rebuild/start if startup fails.",
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
			if startEnsure {
				return ensureNewApplication(identifier, port)
			}
			return err
		}

		name, usePort := DetermineAppParameters(appInfo, cmd, port)

		if startEnsure && appInfo.Status == "running" {
			fmt.Printf("✓ Application '%s' (ID: %s) is already running on port %d\n", name, appInfo.ID, appInfo.Port)
			return nil
		}

		fmt.Printf("Starting application '%s' (ID: %s) on port %d\n", name, appInfo.ID, usePort)

		if err := app.Manager.StartApplication(appInfo.ID, usePort, name); err != nil {
			if !startEnsure {
				return fmt.Errorf("⚠ Failed to start application '%s' (ID: %s): %v", name, appInfo.ID, err)
			}

			fmt.Printf("⚠ Start failed for application '%s' (ID: %s): %v\n", name, appInfo.ID, err)
			fmt.Printf("• Rebuilding application '%s' (ID: %s)\n", name, appInfo.ID)

			if err := stopExistingApp(appInfo); err != nil {
				return err
			}

			if err := rebuildApp(appInfo.ID); err != nil {
				return err
			}

			if err := app.Manager.StartApplication(appInfo.ID, usePort, name); err != nil {
				return fmt.Errorf("⚠ Failed to start rebuilt application '%s' (ID: %s): %v", name, appInfo.ID, err)
			}

			fmt.Printf("✓ Application '%s' (ID: %s) rebuilt and started successfully on port %d\n", name, appInfo.ID, usePort)
			return nil
		}

		fmt.Printf("✓ Application '%s' (ID: %s) started successfully on port %d\n", name, appInfo.ID, usePort)
		return nil
	},
}

func init() {
	StartCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the application on")
	StartCmd.Flags().BoolVar(&startEnsure, "ensure", false, "Build if missing and rebuild if startup fails")
}

func ensureNewApplication(name string, port int) error {
	if err := validateName(name); err != nil {
		return err
	}

	if err := validateUniqueName(name); err != nil {
		return err
	}

	id := app.Manager.GenerateAppID()
	fmt.Printf("• Application '%s' does not exist. Building it with ID: %s\n", name, id)

	if err := createAppEntry(id, name); err != nil {
		return err
	}

	if err := buildApplication(id); err != nil {
		return err
	}

	if err := startApplicationOnPort(id, name, port); err != nil {
		return err
	}

	fmt.Printf("✓ Application '%s' (ID: %s) built and started successfully on port %d\n", name, id, port)
	return nil
}
