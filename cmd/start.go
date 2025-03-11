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
		fmt.Printf("Starting application %s on port %d\n", id, port)
		if err := app.Manager.StartApplication(id, port); err != nil {
			fmt.Printf("Failed to start application %s: %v\n", id, err)
		}
	},
}

func init() {
	StartCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the application on")
}
