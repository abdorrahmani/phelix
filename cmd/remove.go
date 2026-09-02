package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var RemoveCmd = &cobra.Command{
	Use:   "remove [ID|AppName]",
	Short: "Remove an application by its ID or AppName.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix remove <ID|AppName>")
			}
			identifier, err := PromptApp(false, "Select application to remove")
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

		fmt.Printf("• Removing application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
		if err := app.Manager.RemoveApplication(appInfo.ID); err != nil {
			if rerr := phelixgrpc.ReportEventResult(appInfo.ID, appInfo.Name, "remove", false, err.Error(), 0, "", ""); rerr != nil {
				fmt.Printf("%s Dashboard was not updated about the failed removal: %v\n", color.YellowString("⚠"), rerr)
			}
			return phelixerr.Wrap(
				phelixerr.CodeProcessFailed,
				fmt.Sprintf("failed to remove application %q (ID: %s)", appInfo.Name, appInfo.ID),
				err,
			)
		}

		fmt.Printf("✓ Application '%s' (ID: %s) removed successfully\n", appInfo.Name, appInfo.ID)
		// Tell the backend to delete its copy of this app (action="remove" is
		// what triggers the database deletion there). Surface delivery
		// failures instead of failing silently — a silent drop leaves the app
		// stuck on the dashboard forever.
		if rerr := phelixgrpc.ReportEventResult(appInfo.ID, appInfo.Name, "remove", true, "", 0, "", ""); rerr != nil {
			fmt.Printf("%s Removed locally, but the dashboard was NOT updated — the app may still appear there: %v\n", color.YellowString("⚠"), rerr)
		}
		return nil
	},
}
