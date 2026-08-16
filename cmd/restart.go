package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/spf13/cobra"
)

var RestartCmd = &cobra.Command{
	Use:   "restart <ID|AppName>",
	Short: "Restart the application by its ID or AppName.",
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

		fmt.Printf("• Restarting Application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
		if err := app.Manager.RestartApplication(appInfo.ID); err != nil {
			phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", false, err.Error(), 0, "", "")
			return phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to restart application %q (ID: %s)",
				appInfo.Name,
				appInfo.ID,
			)
		}

		fmt.Printf("✓ Application '%s' (ID: %s) restarted successfully\n", appInfo.Name, appInfo.ID)
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", true, "", 0, "", "")
		return nil
	},
}
