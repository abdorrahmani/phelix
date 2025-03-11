package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"os"
)

var StatusCmd = &cobra.Command{
	Use:   "status <ID>",
	Short: "Displays the status of a specific application by its ID",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]
		status, err := app.Manager.StatusApplication(id)
		if err != nil {
			fmt.Printf("Error getting status: %v\n", err)
			return
		}

		// Convert RAM usage from bytes to MB
		ramUsageMB := float64(status.RAMUsage) / (1024 * 1024)

		statusText := status.Status
		if status.Status == "running" {
			statusText = color.GreenString("running")
		} else if status.Status == "stopped" {
			statusText = color.RedString("stopped")
		}

		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"ID", "Status", "PID", "Uptime", "RAM Usage (MB)", "CPU Usage (%)"})
		table.SetBorder(true)
		table.SetRowLine(true)

		table.Append([]string{
			status.ID,
			statusText,
			fmt.Sprintf("%d", status.PID),
			status.Uptime,
			fmt.Sprintf("%.2f", ramUsageMB),
			fmt.Sprintf("%.2f", status.CPUUsage),
		})
		table.Render()
	},
}
