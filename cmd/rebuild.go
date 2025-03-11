package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
	"os/exec"
)

var RebuildCmd = &cobra.Command{
	Use:   "rebuild <ID>",
	Short: "Rebuilds and runs a Go Application",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]
		fmt.Printf("Rebuilding application %s\n", id)

		// Stop the existing app
		app.Manager.StopApplication(id)

		if err := exec.Command("go", "get", "-o", fmt.Sprintf("app_%s", id)).Run(); err != nil {
			fmt.Println("Rebuild failed", err)
			return
		}

		// Get the port from the app's state
		_, err := app.Manager.StatusApplication(id)
		if err != nil {
			fmt.Println("Failed to get app status:", err)
			return
		}
		port := 8080 // Default port if not found
		if appInfo, exists := app.Manager.(*app.AppManager).Apps[id]; exists {
			port = appInfo.Port
		}
		app.Manager.StartApplication(id, port)
	},
}
