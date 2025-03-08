package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var RestartCmd = &cobra.Command{
	Use:   "restart <ID>",
	Short: "Restart the application by its ID",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]
		fmt.Printf("Restarting Application %s\n", id)
		if err := app.Manager.RestartApplication(id); err != nil {
			fmt.Printf("Restart failed: %s\n", err)
			return
		}
		fmt.Printf("Application %s restarted Successfully\n", id)
	},
}
