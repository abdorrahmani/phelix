package cmd

import (
	"fmt"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var StopCmd = &cobra.Command{
	Use:   "stop <ID>",
	Short: "Stop a running application by its ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load app state: %v", err)
		}

		if err := app.Manager.StopApplication(id); err != nil {
			return fmt.Errorf("failed to stop application: %v", err)
		}

		fmt.Printf("Gophel: Application %s stopped successfully\n", id)
		return nil
	},
}
