package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
	"os"
	"text/tabwriter"
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

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(w, "ID\tStatus\tPID\tUptime\tRAM Usage (bytes)\tCPU Usage (%)")
		fmt.Fprintln(w, "----\t------\t----\t------\t-----------------\t-------------")
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%.2f\n",
			status.ID, status.Status, status.PID, status.Uptime, status.RAMUsage, status.CPUUsage)
		w.Flush()
	},
}
