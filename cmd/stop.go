package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/spf13/cobra"
)

var StopCmd = &cobra.Command{
	Use:   "stop [ID|AppName]",
	Short: "Stop a running application by its ID or AppName.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix stop <ID|AppName>")
			}
			identifier, err := PromptApp(false, "Select application to stop")
			if err != nil {
				return err
			}
			args = []string{identifier}
		}

		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		if err := app.Manager.StopApplication(appInfo.ID); err != nil {
			phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "stop", false, err.Error(), 0, "", "")
			return phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to stop application %q (ID: %s)",
				appInfo.Name,
				appInfo.ID,
			)
		}

		fmt.Printf("✓ Application '%s' (ID: %s) stopped successfully\n", appInfo.Name, appInfo.ID)
		phelixgrpc.ReportEvent(appInfo.ID, appInfo.Name, "stop", true, "", 0, "", "")
		return nil
	},
}
