package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var watchDisable bool

// WatchCmd toggles an application's participation in backend monitoring.
//
// Watching is a per-app opt-in (default: disabled). A watched app sends its
// monitoring data — app metrics, health snapshots, app logs, deployment
// topology, dashboard registration — to the Phelix backend; an unwatched app
// sends none of it while keeping every local capability (build, run, stop,
// rollback, local status and health) unchanged. Server-level data (server
// identity, server metrics, self logs, agent metadata) flows only while at
// least one app is watched: a server whose every app has opted out transmits
// nothing to the backend at all.
//
// The value persists in apps.json, and a running monitor daemon picks the
// change up on its next reporting tick — no restart needed. The watching:
// key in the project's phelix.yaml is updated too, so the next build/rebuild
// keeps the new state instead of reverting it.
var WatchCmd = &cobra.Command{
	Use:   "watch <ID|AppName>",
	Short: "Enable or disable backend monitoring (watching) for an application",
	Long: "Enable or disable backend monitoring (watching) for an application.\n" +
		"\n" +
		"Watching controls whether monitoring data is sent to the Phelix backend:\n" +
		"a watched app sends its metrics, health, logs and deployment topology,\n" +
		"and while at least one app is watched the server sends its own data too\n" +
		"(identity, host metrics, self logs). When no app is watched, nothing is\n" +
		"transmitted. It never disables local functionality. Default is disabled.\n" +
		"\n" +
		"The toggle persists in apps.json and is also written to the watching:\n" +
		"key of the project's phelix.yaml (when present), so build/rebuild keeps it.",
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

		enabled := !watchDisable
		appInfo.Watching = enabled
		if err := app.Manager.SaveState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to save app state", err)
		}

		if enabled {
			fmt.Printf("✓ Watching enabled for application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
			fmt.Println("  Monitoring data (metrics, health, logs) will be sent to the backend")
		} else {
			fmt.Printf("✓ Watching disabled for application '%s' (ID: %s)\n", appInfo.Name, appInfo.ID)
			fmt.Println("  No monitoring data will be sent to the backend; local functionality is unaffected")
		}

		// phelix.yaml is the desired state the next build/rebuild converges on
		// (syncProjectWatching); keep it in sync so a rebuild never reverts
		// this toggle. Best-effort: the runtime flag above is already saved.
		if appInfo.Directory != "" {
			switch err := project.SetWatching(appInfo.Directory, enabled); {
			case err == nil:
				fmt.Printf("  Updated watching: %s in %s\n", watchingWord(enabled),
					color.CyanString(filepath.Join(appInfo.Directory, project.FileName)))
			case phelixerr.CodeOf(err) == phelixerr.CodeNotFound:
				// No phelix.yaml in the project directory: nothing to update —
				// the runtime flag is the only state, and a rebuild without a
				// yaml leaves it untouched.
				fmt.Printf("  No %s in %s — only the runtime state was changed\n",
					color.CyanString(project.FileName), appInfo.Directory)
			default:
				fmt.Printf("  %s Warning: could not update %s: %v\n",
					color.YellowString("⚠"), project.FileName, err)
			}
		}
		return nil
	},
}

func watchingWord(enabled bool) string {
	if enabled {
		return project.WatchingEnable
	}
	return project.WatchingDisable
}

func init() {
	WatchCmd.Flags().BoolVar(&watchDisable, "disable", false, "Disable watching instead of enabling it")
}
