package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deployanalysis"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	deploymentReportLimit    int
	deploymentReportVersion  string
	deploymentReportLatest   bool
	deploymentReportDetailed bool
	deploymentReportJSON     bool
)

// DeploymentReportCmd is a read-only inspection command for stored deployment
// analyses. It never deploys, never triggers a rollback and works offline.
var DeploymentReportCmd = &cobra.Command{
	Use:           "deployment-report [AppName]",
	Short:         "Show stored deployment impact analyses for an app (read-only)",
	Long:          `Displays the stored deployment analyses for an application: version transition, deployment outcome, impact score and per-metric comparisons. Read-only: never deploys, never rolls back and requires no network. Pending analyses whose observation window has elapsed are finalized on read.`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		var identifier string
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument,
					"missing required <AppName>; usage: phelix deployment-report <AppName> [--latest|--version vN] [--detailed]")
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

		if _, err := deployanalysis.FinalizeDue(appName, deployanalysis.DefaultConfig()); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not finalize pending analyses: %v\n", err)
		}
		phelixgrpc.SyncDeploymentAnalyses(appName)

		if deploymentReportVersion != "" {
			rep, err := deployanalysis.FindByVersion(appName, deploymentReportVersion)
			if err != nil {
				return err
			}
			if rep == nil {
				return phelixerr.Newf(phelixerr.CodeNotFound,
					"no deployment analysis recorded for version %s of %s", deploymentReportVersion, appName)
			}
			return printDeploymentReports([]deployanalysis.Report{*rep}, true)
		}

		reports, err := deployanalysis.ListReports(appName)
		if err != nil {
			return err
		}
		if deploymentReportLatest && len(reports) > 0 {
			reports = reports[:1]
		}
		if deploymentReportDetailed && len(reports) > 1 {
			reports = reports[:1]
		}
		return printDeploymentReports(reports, false)
	},
}

func printDeploymentReports(reports []deployanalysis.Report, forceDetail bool) error {
	if deploymentReportJSON {
		payload := any(reports)
		if len(reports) == 1 {
			payload = reports[0]
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(payload)
	}

	if len(reports) == 0 {
		fmt.Println("No deployment analyses recorded yet.")
		return nil
	}

	detailed := deploymentReportDetailed || deploymentReportLatest || forceDetail
	if detailed {
		deployanalysis.PrintReport(os.Stdout, &reports[0], deploymentReportDetailed || forceDetail)
		return nil
	}

	limit := deploymentReportLimit
	if limit <= 0 {
		limit = 10
	}
	if len(reports) > limit {
		reports = reports[:limit]
	}
	fmt.Printf("%s Deployment analyses (%d most recent)\n", color.BlueString("→"), len(reports))
	deployanalysis.PrintList(os.Stdout, reports[0].AppName, reports)
	return nil
}

func init() {
	DeploymentReportCmd.Flags().IntVar(&deploymentReportLimit, "limit", 10, "How many most-recent deployment analyses to list")
	DeploymentReportCmd.Flags().StringVar(&deploymentReportVersion, "version", "", "Show the analysis for one target version (e.g. v14)")
	DeploymentReportCmd.Flags().BoolVar(&deploymentReportLatest, "latest", false, "Show the most recent analysis")
	DeploymentReportCmd.Flags().BoolVar(&deploymentReportDetailed, "detailed", false, "Show per-metric comparisons for the selected analysis")
	DeploymentReportCmd.Flags().BoolVar(&deploymentReportJSON, "json", false, "Print machine-readable JSON")
}
