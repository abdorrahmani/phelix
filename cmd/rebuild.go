package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
	"os/exec"
)

var RebuildCmd = &cobra.Command{
	Use:   "rebuild <ID>",
	Short: "Rebuilds and runs a Go Application",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]
		fmt.Printf("Rebuilding application %s\n", id)

		// Stop the existing app
		app.StopApplication(id)

		if err := exec.Command("go", "get", "-o", fmt.Sprintf("app_%s", id)).Run(); err != nil {
			fmt.Println("Rebuild failed", err)
			return
		}

		// Restart
		app.StartApplication(id)
	},
}
