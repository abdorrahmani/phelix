package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/spf13/cobra"
)

var RestartCmd = &cobra.Command{
	Use:   "restart [ID|AppName]",
	Short: "Restart the application by its ID or AppName.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix restart <ID|AppName>")
			}
			identifier, err := PromptApp(false, "Select application to restart")
			if err != nil {
				return err
			}
			args = []string{identifier}
		}

		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		// Zero-downtime deployments restart through their own teardown +
		// restore path so the app never downgrades to a classic process
		// binding the public port.
		if loadDeployState(appInfo.Name) != nil {
			fmt.Printf("• Restarting deployment for '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
			if err := runDeployAwareStop(appInfo); err != nil {
				phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", false, err.Error(), 0, "", "")
				return phelixerr.Wrapf(
					phelixerr.CodeProcessFailed,
					err,
					"failed to stop deployment for %q (ID: %s)",
					appInfo.Name, appInfo.ID,
				)
			}
			if err := runDeployAwareStart(appInfo); err != nil {
				phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", false, err.Error(), 0, "", "")
				return phelixerr.Wrapf(
					phelixerr.CodeProcessFailed,
					err,
					"failed to restore deployment for %q (ID: %s)",
					appInfo.Name, appInfo.ID,
				)
			}
			fmt.Printf("✓ Application '%s' (ID: %s) restarted successfully\n", appInfo.Name, appInfo.ID)
			phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "restart", true, "", 0, "", "")
			return nil
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
