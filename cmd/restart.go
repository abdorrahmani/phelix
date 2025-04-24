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
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load state: %v", err)
		}

		appInfo, err := GetAppInfo(id)
		if err != nil {
			return err
		}

		fmt.Printf("Restarting Application '%s' (ID: %s)\n", appInfo.Name, id)
		if err := app.Manager.RestartApplication(id); err != nil {
			return fmt.Errorf("restart failed: %v", err)
		}

		fmt.Printf("Application '%s' (ID: %s) restarted successfully\n", appInfo.Name, id)
		return nil
	},
}
