package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var StopCmd = &cobra.Command{
	Use:   "stop <ID>",
	Short: "Stops a specific application by its ID",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]
		fmt.Printf("Stopping application %s\n", id)

		if err := app.Manager.StopApplication(id); err != nil {
			fmt.Printf("Failed to stop application %s: %v\n", id, err)
		}
	},
}
