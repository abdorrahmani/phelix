package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"os"
)

var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "Lists all applications with their ID, status, PID, and uptime",
	Run: func(cmd *cobra.Command, args []string) {
		apps := app.Manager.ListApplications()
		if len(apps) == 0 {
			fmt.Println("No applications found.")
			return
		}

		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"ID", "Name", "Status", "PID", "Uptime"})
		table.SetBorder(true)
		table.SetRowLine(true)

		table.SetColumnAlignment([]int{tablewriter.ALIGN_LEFT, tablewriter.ALIGN_CENTER, tablewriter.ALIGN_CENTER, tablewriter.ALIGN_CENTER})

		for _, app := range apps {
			status := app.Status
			if status == "running" {
				status = color.GreenString("running")
			} else if status == "stopped" {
				status = color.RedString("stopped")
			}

			table.Append([]string{app.ID, color.BlueString(app.Name), status, fmt.Sprintf("%d", app.PID), app.Uptime})
		}
		table.Render()
	},
}
