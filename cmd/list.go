package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
	"os"
	"text/tabwriter"
)

var ListCmd = &cobra.Command{
	Use:   "list",
	Short: "Lists all applications with their ID, status, PID, and uptime",
	Run: func(cmd *cobra.Command, args []string) {
		apps := app.ListApplications()
		if len(apps) == 0 {
			fmt.Println("No applications found.")
			return
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(w, "ID\tStatus\tPID\tUptime")
		fmt.Fprintln(w, "------\t------\t------\t------")

		for _, app := range apps {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", app.ID, app.Status, app.PID, app.Uptime)
		}
		w.Flush()
	},
}
