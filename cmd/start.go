package cmd

import (
	"fmt"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var port int

var StartCmd = &cobra.Command{
	Use:   "start <ID> --port <PORT>",
	Short: "Starts a specific application by its ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load state: %v", err)
		}

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
