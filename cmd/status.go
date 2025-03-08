package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var StatusCmd = &cobra.Command{
	Use:   "status <ID>",
	Short: "Displays the status of a specific application by its ID",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]
		status, err := app.Manager.StatusApplication(id)
		if err != nil {
			fmt.Println("Error getting status:", err)
			return
		}
		fmt.Printf("Status of application %s: %s\n", id, status)
	},
}
