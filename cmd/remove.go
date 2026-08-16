package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/spf13/cobra"
)

var RemoveCmd = &cobra.Command{
	Use:   "remove <ID|AppName>",
	Short: "Remove an application by its ID or AppName.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		fmt.Printf("• Removing application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
		if err := app.Manager.RemoveApplication(appInfo.ID); err != nil {
			phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "remove", false, err.Error(), 0, "", "")
			return phelixerr.Wrap(
				phelixerr.CodeProcessFailed,
				fmt.Sprintf("failed to remove application %q (ID: %s)", appInfo.Name, appInfo.ID),
				err,
			)
		}

		fmt.Printf("✓ Application '%s' (ID: %s) removed successfully\n", appInfo.Name, appInfo.ID)
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "remove", true, "", 0, "", "")
		return nil
	},
}
