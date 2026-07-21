package cmd

import (
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var StatusCmd = &cobra.Command{
	Use:   "status <ID|AppName>",
	Short: "Displays the status of a specific application by its ID or AppName",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("failed to load state: %v", err)
		}

		status, err := app.Manager.StatusApplication(identifier)
		if err != nil {
			return fmt.Errorf("error getting status: %v", err)
		}

		displayStatus(status)
		return nil
	},
}

func displayStatus(status app.AppStatus) {
	table := createStatusTable()
	populateStatusTable(table, status)
	table.Render()
}

func createStatusTable() *tablewriter.Table {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"ID", "Name", "Language", "Status", "PID", "Uptime", "RAM Usage (MB)", "CPU Usage (%)"})
	table.SetBorder(true)
	table.SetRowLine(true)
	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_LEFT,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})
	return table
}

func populateStatusTable(table *tablewriter.Table, status app.AppStatus) {
	statusText := FormatStatus(status.Status)
	ramUsageMB := float64(status.RAMUsage) / (1024 * 1024)
	lang := status.Language
	if lang == "" {
		lang = "unknown"
	}

	table.Append([]string{
		status.ID,
		color.BlueString(status.Name),
		color.CyanString(lang),
		statusText,
		fmt.Sprintf("%d", status.PID),
		status.Uptime,
		fmt.Sprintf("%.2f", ramUsageMB),
		fmt.Sprintf("%.2f", status.CPUUsage),
	})
}
