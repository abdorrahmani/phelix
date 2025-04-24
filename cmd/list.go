package cmd

import (
	"fmt"
	"os"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "Lists all applications with their ID, status, PID, and uptime",
	RunE: func(cmd *cobra.Command, args []string) error {
		apps := app.Manager.ListApplications()
		if len(apps) == 0 {
			fmt.Println("No applications found.")
			return nil
		}

		table := createTable()
		populateTable(table, apps)
		table.Render()
		return nil
	},
}

func createTable() *tablewriter.Table {
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"ID", "Name", "Status", "PID", "Uptime"})
	table.SetBorder(true)
	table.SetRowLine(true)
	table.SetColumnAlignment([]int{
		tablewriter.ALIGN_LEFT,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
		tablewriter.ALIGN_CENTER,
	})
	return table
}

func populateTable(table *tablewriter.Table, apps []app.AppListItem) {
	for _, app := range apps {
		status := FormatStatus(app.Status)
		table.Append([]string{
			app.ID,
			color.BlueString(app.Name),
			status,
			fmt.Sprintf("%d", app.PID),
			app.Uptime,
		})
	}
}
