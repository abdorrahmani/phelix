package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var StartCmd = &cobra.Command{
	Use:   "start <ID> --port <PORT>",
	Short: "Starts a specific application by its ID",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]

		if err := app.Manager.LoadState(); err != nil {
			fmt.Printf("Failed to load state: %v\n", err)
			return
		}
		existingApp, exists := app.Manager.(*app.AppManager).Apps[id]
		name := id
		usePort := port
		if exists {
			if existingApp.Name != "" {
				name = existingApp.Name
			}

			if !cmd.Flags().Changed("port") && existingApp.Port != 0 {
				usePort = existingApp.Port
			}
		}

		fmt.Printf("Starting application '%s' (ID: %s) on port %d\n", name, id, usePort)
		if err := app.Manager.StartApplication(id, usePort, name); err != nil {
			fmt.Printf("Failed to start application '%s' (ID: %s): %v\n", name, id, err)
		}
	},
}

func init() {
	StartCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the application on")
}
