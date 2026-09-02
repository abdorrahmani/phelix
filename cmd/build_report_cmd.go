package cmd

import (
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var buildReportLimit int

// BuildReportCmd is a read-only inspection command: it shows the stored build
// report history from versions.json without building anything, fully offline.
var BuildReportCmd = &cobra.Command{
	Use:           "build-report [AppName]",
	Short:         "Show stored build reports for an app (read-only)",
	Long:          `Displays recent stored build reports for an application — compiler, duration, cache status and binary size per version — straight from its versions.json. Read-only: never triggers a build and requires no network.`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		var identifier string
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument,
					"missing required <AppName>; usage: phelix build-report <AppName> [--limit N]")
			}
			chosen, err := PromptApp(false, "Select application")
			if err != nil {
				return err
			}
			identifier = chosen
		} else {
			identifier = args[0]
		}

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}
		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}
		appName := appInfo.Name

		limit := buildReportLimit
		if limit <= 0 {
			limit = 5
		}

		vers, err := deploy.ListVersionsForDisplay(appName, deploy.DefaultRetention{Max: 5})
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load version metadata", err)
		}
		if len(vers) == 0 {
			fmt.Printf("  No versioned builds recorded for %s yet.\n", color.CyanString("'%s'", appName))
			return nil
		}

		views := make([]buildreport.StoredReportView, 0, limit)
		for _, v := range vers {
			if len(views) >= limit {
				break
			}
			views = append(views, buildreport.StoredReportView{
				Version: v.Version,
				Tag:     v.Tag,
				Commit:  v.GitCommit,
				BuiltAt: v.BuiltAt,
				Report:  v.BuildReport,
			})
		}

		fmt.Printf("%s Build reports for %s (%d most recent)\n",
			color.BlueString("→"), color.CyanString("'%s'", appName), len(views))
		buildreport.PrintStoredReports(os.Stdout, appName, views)
		return nil
	},
}

func init() {
	BuildReportCmd.Flags().IntVar(&buildReportLimit, "limit", 5, "How many most-recent build reports to show")
}
