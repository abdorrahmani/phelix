package cmd

import (
	"fmt"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/spf13/cobra"
)

var watchDisable bool

// WatchCmd toggles an application's participation in backend monitoring.
//
// Watching is a per-app opt-in (default: disabled). A watched app sends its
// monitoring data — app metrics, health snapshots, app logs, deployment
// topology, dashboard registration — to the Phelix backend; an unwatched app
// sends none of it while keeping every local capability (build, run, stop,
// rollback, local status and health) unchanged. Server-level monitoring is
// never affected by this flag.
//
// The value persists in apps.json, and a running monitor daemon picks the
// change up on its next reporting tick — no restart needed.
var WatchCmd = &cobra.Command{
	Use:   "watch <ID|AppName>",
	Short: "Enable or disable backend monitoring (watching) for an application",
	Long: "Enable or disable backend monitoring (watching) for an application.\n" +
		"\n" +
		"Watching only controls whether the app's monitoring data is sent to the\n" +
		"Phelix backend. It never disables local functionality, and server-level\n" +
		"monitoring continues regardless. Default is disabled.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		appInfo.Watching = !watchDisable
		if err := app.Manager.SaveState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to save app state", err)
		}

		if appInfo.Watching {
			fmt.Printf("✓ Watching enabled for application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
			fmt.Println("  Monitoring data (metrics, health, logs) will be sent to the backend")
		} else {
			fmt.Printf("✓ Watching disabled for application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
			fmt.Println("  No monitoring data will be sent to the backend; local functionality is unaffected")
		}
		return nil
	},
}

func init() {
	WatchCmd.Flags().BoolVar(&watchDisable, "disable", false, "Disable watching instead of enabling it")
}
