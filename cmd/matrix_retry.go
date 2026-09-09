package cmd

import (
	"fmt"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// matrixRetryCmd implements `phelix matrix retry <run-id> --failed`: manual
// retry of a completed run's failed combinations. Unlike automatic retries
// (same run, same combination, extra attempts) and resume (same run,
// incomplete combinations), a manual retry creates a NEW run that records its
// parent — the original run's history is never modified.
var matrixRetryCmd = &cobra.Command{
	Use:   "retry <run-id>",
	Short: "Retry a Run's failed combinations in a new, linked Run",
	Long: `Creates a new Matrix Run that executes only the failed combinations
of an existing Run, using the original Run's configuration snapshot (not the
current phelix.yaml). The original Run is left unchanged; the new Run records
its parent for inspection.

Currently --failed is the only selector: it retries every combination whose
final status in the source Run is failed.`,
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !matrixRetryFailed {
			return phelixerr.New(phelixerr.CodeInvalidArgument,
				"specify a selection: --failed retries the failed combinations (more selectors may be added later)")
		}
		id, err := matrix.ParseRunID(strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}
		source, err := matrix.LoadRun(id)
		if err != nil {
			return err
		}

		combos := source.FailedCombinations()
		if len(combos) == 0 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix run %s has no failed combinations — nothing to retry (status: %s)", source.ID, source.Status)
		}
		if source.ProjectDir == "" {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix run %s has no project directory recorded — it predates run snapshots and cannot be retried", source.ID)
		}

		fmt.Printf("%s Retrying failed combinations of %s (%s): %d of %d\n",
			color.BlueString("→"), color.CyanString(string(source.ID)), source.AppName, len(combos), source.Total)
		for _, c := range combos {
			fmt.Printf("    %s %s\n", color.New(color.Faint).Sprint("•"), c.ID())
		}
		fmt.Println()

		// The retry run executes the source run's effective configuration —
		// including its automatic-retry budget — and links back to it.
		retryID := matrix.NewUniqueRunID(time.Now())
		retryRun := matrix.NewRetryRun(retryID, source, time.Now())
		retryRun.InitCombinations(combos)
		fmt.Printf("%s Retry Run: %s (parent: %s)\n",
			color.BlueString("→"), color.CyanString(string(retryID)), color.CyanString(string(source.ID)))

		if serr := matrix.SaveRun(retryRun); serr != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "could not record retry run", serr)
		}

		executed, interrupted, err := startMatrixSession(retryRun, combos, retryRun.Config.BuildArgs, buildDebug)
		if err != nil {
			return err
		}
		completeMatrixSession(retryRun, executed, "")

		switch {
		case interrupted:
			return phelixerr.Newf(phelixerr.CodeBuildFailed,
				"retry run %s interrupted — continue it with 'phelix build --matrix --resume=%s'",
				retryID, retryID)
		case retryRun.Failed > 0:
			return phelixerr.Newf(phelixerr.CodeBuildFailed,
				"retry run %s completed with %d failure(s) out of %d combinations", retryID, retryRun.Failed, retryRun.Total)
		}
		fmt.Printf("%s Retry run %s succeeded — see 'phelix matrix show %s'\n",
			color.GreenString("✓"), retryID, retryID)
		return nil
	},
}

var matrixRetryFailed bool

func init() {
	matrixRetryCmd.Flags().BoolVar(&matrixRetryFailed, "failed", false, "Retry the combinations whose final status in the source run is failed")
	MatrixCmd.AddCommand(matrixRetryCmd)
}
