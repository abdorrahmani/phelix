package cmd

import (
	"fmt"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var StopCmd = &cobra.Command{
	Use:   "stop <ID|AppName>",
	Short: "Stop a running application by its ID or AppName.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf(" ⚠ Failed to load app state: %v", err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		if err := app.Manager.StopApplication(appInfo.ID); err != nil {
			return fmt.Errorf(" ⚠ Failed to stop application '%s' (ID: %s) : %v", appInfo.Name, appInfo.ID, err)
		}

		fmt.Printf("✓ Application '%s' (ID: %s) stopped successfully\n", appInfo.Name, appInfo.ID)
		return nil
	},
}
