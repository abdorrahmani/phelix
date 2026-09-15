package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/webhook"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"github.com/spf13/cobra"
)

var (
	webhookStatusJSON   bool
	webhookHistoryJSON  bool
	webhookHistoryLimit int
)

// webhookJobsDir is the durable deployment-job store location.
func webhookJobsDir() string {
	return filepath.Join(server.DataDir(), "webhook", "jobs")
}

var webhookStatusCmd = &cobra.Command{
	Use:   "status <app>",
	Short: "Show active webhook deployment jobs for an app",
	Long: `Show the in-flight webhook deployment jobs for an application.

Reads the local durable job store (~/.phelix/webhook/jobs); works offline and
requires no authentication. Jobs that finished (succeeded, failed, rolled
back, cancelled) are shown by 'phelix webhook history'.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cobraCmd *cobra.Command, args []string) error {
		return runWebhookJobsStatus(args[0], webhookStatusJSON)
	},
}

var webhookHistoryCmd = &cobra.Command{
	Use:   "history <app>",
	Short: "Show recent webhook deployment history for an app",
	Long: `Show the most recent finished webhook deployments for an application
(succeeded, failed, rolled back, cancelled — newest first).

Reads the local durable job store; works offline and requires no
authentication.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cobraCmd *cobra.Command, args []string) error {
		return runWebhookJobsHistory(args[0], webhookHistoryLimit, webhookHistoryJSON)
	},
}

func init() {
	webhookStatusCmd.Flags().BoolVar(&webhookStatusJSON, "json", false, "Output machine-readable JSON")
	webhookHistoryCmd.Flags().BoolVar(&webhookHistoryJSON, "json", false, "Output machine-readable JSON")
	webhookHistoryCmd.Flags().IntVar(&webhookHistoryLimit, "limit", 20, "Maximum number of jobs to show")
	WebhookCmd.AddCommand(webhookStatusCmd)
	WebhookCmd.AddCommand(webhookHistoryCmd)
}

func runWebhookJobsStatus(app string, asJSON bool) error {
	store := webhook.OpenJobStoreReadOnly(webhookJobsDir())
	if err := store.Load(); err != nil {
		return err
	}
	active := store.ActiveForApp(app)
	if asJSON {
		return renderWebhookJobsJSON(os.Stdout, active)
	}
	if len(active) == 0 {
		fmt.Println("No active webhook deployments.")
		return nil
	}
	fmt.Printf("Webhook Deployments: %s\n\n", color.CyanString(app))
	renderWebhookStatusTable(os.Stdout, active)
	return nil
}

func runWebhookJobsHistory(app string, limit int, asJSON bool) error {
	store := webhook.OpenJobStoreReadOnly(webhookJobsDir())
	if err := store.Load(); err != nil {
		return err
	}
	history := store.HistoryForApp(app, limit)
	if asJSON {
		return renderWebhookJobsJSON(os.Stdout, history)
	}
	if len(history) == 0 {
		fmt.Println("No webhook deployment history.")
		return nil
	}
	renderWebhookHistoryTable(os.Stdout, history)
	return nil
}

func newWebhookJobsTable(w io.Writer) *tablewriter.Table {
	table := tablewriter.NewTable(w,
		tablewriter.WithRendition(tw.Rendition{
			Settings: tw.Settings{Separators: tw.Separators{BetweenRows: tw.On}},
		}),
		tablewriter.WithConfig(tablewriter.Config{
			Header: tw.CellConfig{
				Alignment: tw.CellAlignment{Global: tw.AlignCenter},
			},
			Row: tw.CellConfig{
				Alignment: tw.CellAlignment{
					Global:    tw.AlignCenter,
					PerColumn: []tw.Align{tw.AlignLeft},
				},
			},
		}),
	)
	return table
}

func renderWebhookStatusTable(w io.Writer, recs []webhook.JobRecord) {
	table := newWebhookJobsTable(w)
	table.Header([]string{"Job", "Status", "Stage", "Commit", "Version"})
	for _, rec := range recs {
		table.Append([]string{
			rec.ID,
			statusColored(rec.Status),
			rec.Stage,
			shortCommit(rec.Commit),
			versionColumn(rec.Version),
		})
	}
	table.Render()
}

func renderWebhookHistoryTable(w io.Writer, recs []webhook.JobRecord) {
	table := newWebhookJobsTable(w)
	table.Header([]string{"Time", "Status", "Commit", "Version", "Message"})
	for _, rec := range recs {
		message := rec.ErrorMessage
		if message == "" {
			message = historyMessage(rec)
		}
		table.Append([]string{
			time.UnixMilli(rec.FinishedAt).Format("2006-01-02 15:04"),
			statusColored(rec.Status),
			shortCommit(rec.Commit),
			versionColumn(rec.Version),
			truncate(message, 60),
		})
	}
	table.Render()
}

// renderWebhookJobsJSON writes machine-readable job records (the same fields
// the store persists; no secrets, no worktree paths).
func renderWebhookJobsJSON(w io.Writer, recs []webhook.JobRecord) error {
	if recs == nil {
		recs = []webhook.JobRecord{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(recs)
}

func statusColored(status string) string {
	switch status {
	case webhook.StatusSucceeded:
		return color.GreenString(status)
	case webhook.StatusFailed:
		return color.RedString(status)
	case webhook.StatusRolledBack:
		return color.YellowString(status)
	case webhook.StatusCancelled:
		return color.HiBlackString(status)
	default:
		return color.BlueString(status)
	}
}

func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

func versionColumn(version int) string {
	if version > 0 {
		return fmt.Sprintf("v%d", version)
	}
	return "-"
}

func historyMessage(rec webhook.JobRecord) string {
	switch rec.Status {
	case webhook.StatusSucceeded:
		return "deployed"
	case webhook.StatusRolledBack:
		return "rolled back"
	case webhook.StatusCancelled:
		return "cancelled"
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
