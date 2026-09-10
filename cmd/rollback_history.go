package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var rollbackHistoryLimit int

var rollbackHistoryCmd = &cobra.Command{
	Use:           "history [AppName]",
	Short:         "Show rollback history for an application",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if rollbackHistoryLimit <= 0 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"invalid --limit %d: must be a positive number", rollbackHistoryLimit)
		}

		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument,
					"missing required <AppName>; usage: phelix rollback history <AppName> [--limit N]")
			}
			chosen, err := PromptApp(false, "Select application to view rollback history")
			if err != nil {
				return err
			}
			args = []string{chosen}
		}

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}
		appInfo, err := GetAppInfo(args[0])
		if err != nil {
			return err
		}

		return runRollbackHistory(appInfo)
	},
}

func init() {
	rollbackHistoryCmd.Flags().IntVar(&rollbackHistoryLimit, "limit", 20,
		"Maximum number of history entries to show")
	RollbackCmd.AddCommand(rollbackHistoryCmd)
}

// runRollbackHistory renders the rollback history for one app — the newest
// --limit records, newest first, from the per-app structured history file —
// and reports the same structured records to the backend (one "history"
// event), so the dashboard can display history without parsing CLI output.
func runRollbackHistory(appInfo *app.AppInfo) error {
	appName := appInfo.Name
	records, skipped, err := deploy.ReadRollbackHistory(appName)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to read rollback history", err)
	}

	// Newest first. History lines are appended oldest-first; sort on the
	// recorded timestamp so display order never depends on the file being
	// strictly ordered.
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Time.After(records[j].Time)
	})
	if len(records) > rollbackHistoryLimit {
		records = records[:rollbackHistoryLimit]
	}

	// Backend sync: structured records, newest-first, limited — the same
	// slice the table below renders. Read-only; a sync failure never breaks
	// the command.
	emitRollbackHistory(appInfo, records, rollbackHistoryLimit, skipped)

	fmt.Printf("Rollback History — %s\n\n", color.CyanString(appName))

	if len(records) == 0 {
		fmt.Println("No rollback history found.")
		return nil
	}

	if skipped > 0 {
		fmt.Printf("%s %d malformed history %s skipped\n\n",
			color.YellowString("⚠"), skipped, plural(skipped))
	}

	table := tablewriter.NewTable(os.Stdout)
	table.Header([]string{"TIME", "FROM", "TO", "STATUS", "MODE", "SOURCE", "REASON"})
	for _, rec := range records {
		status := strings.ToUpper(rec.Status)
		if rec.Status == deploy.RollbackStatusFailed {
			status = color.RedString(status)
		} else if rec.Status == deploy.RollbackStatusSuccess {
			if rec.Verification != nil && rec.Verification.Status == deploy.RollbackVerifyFailed {
				// Execution succeeded but the stability window failed: the two
				// outcomes must stay distinguishable at a glance.
				status = color.YellowString("VERIFY_FAILED")
			} else {
				status = color.GreenString(status)
			}
		}
		reason := rec.Reason
		if reason == "" {
			// Absent reason: old records never had one, and reason-less
			// rollbacks stay reason-free — both render the missing-value dash.
			reason = "—"
		}
		source := rec.Source
		if source == "" {
			// Records written before the source field existed were manual.
			source = deploy.RollbackSourceManual
		}
		table.Append([]string{
			rec.Time.Format("2006-01-02 15:04:05"),
			rec.From,
			rec.To,
			status,
			rec.Mode,
			source,
			reason,
		})
	}
	table.Render()
	return nil
}

// emitRollbackHistory reports the rendered history slice to the backend as
// structured records. Mirrors the local record semantics: status is the
// EXECUTION outcome, the mode is the strategy that rollback actually used,
// and the verification block carries the window outcome separately.
func emitRollbackHistory(appInfo *app.AppInfo, records []deploy.RollbackHistoryRecord, limit, skipped int) {
	r := phelixgrpc.NewRollbackReporter("", appInfo.ID, appInfo.ID, appInfo.Name, "history", "history", "", "", "")
	r.SetHistory(rollbackHistoryEntries(records))
	if skipped > 0 {
		r.SetMetadata("malformed_skipped", fmt.Sprintf("%d", skipped))
	}
	r.SetMetadata("limit", fmt.Sprintf("%d", limit))
	r.SetMetadata("record_count", fmt.Sprintf("%d", len(records)))
	r.Emit(phelixgrpc.RollbackStepHistory, true, fmt.Sprintf("reported %d rollback history records", len(records)), 0, "")
}

// rollbackHistoryEntries maps local history records onto the wire message.
func rollbackHistoryEntries(records []deploy.RollbackHistoryRecord) []*pb.RollbackHistoryEntry {
	entries := make([]*pb.RollbackHistoryEntry, 0, len(records))
	for _, rec := range records {
		e := &pb.RollbackHistoryEntry{
			Time:        rec.Time.UnixMilli(),
			FromVersion: rec.From,
			ToVersion:   rec.To,
			Status:      rec.Status,
			Mode:        rec.Mode,
			Reason:      rec.Reason,
			Source:      rec.Source,
			Error:       rec.Error,
		}
		if rec.Verification != nil {
			e.VerifyStatus = rec.Verification.Status
			e.VerifyDuration = rec.Verification.Duration
			e.VerifyError = rec.Verification.Error
		}
		entries = append(entries, e)
	}
	return entries
}
