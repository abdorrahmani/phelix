package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
	"os/exec"
)

var rebuildPort int

var RebuildCmd = &cobra.Command{
	Use:   "rebuild <ID> --port<PORT>",
	Short: "Rebuilds and runs a Go Application by its ID",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		id := args[0]

		if err := app.Manager.LoadState(); err != nil {
			fmt.Printf("Failed to load state: %v\n", err)
			return
		}
		appInfo, exists := app.Manager.(*app.AppManager).Apps[id]
		name := id
		portToUse := rebuildPort

		if exists {
			if appInfo.Name != "" {
				name = appInfo.Name
			}

			if !cmd.Flags().Changed("port") && appInfo.Port != 0 {
				portToUse = appInfo.Port
			}
		} else {
			fmt.Printf("Application with ID %s not found in state\n", id)
			return
		}

		fmt.Printf("Rebuilding application '%s' (ID: %s)\n", name, id)

		if err := app.Manager.StopApplication(id); err != nil {

			if !exists || appInfo.Status != "running" {
				fmt.Printf("Note: Application '%s' (ID: %s) was not running\n", name, id)
			} else {
				fmt.Printf("Failed to stop application '%s' (ID: %s): %v\n", name, id, err)
				return
			}
		}

		if err := exec.Command("go", "build", "-o", fmt.Sprintf("app_%s", id)).Run(); err != nil {
			fmt.Printf("Rebuild failed for '%s' (ID: %s): %v\n", name, id, err)
			return
		}

		if err := app.Manager.StartApplication(id, portToUse, name); err != nil {
			fmt.Printf("Failed to start rebuilt application '%s' (ID: %s): %v\n", name, id, err)
			return
		}
	},
}

func init() {
	RebuildCmd.Flags().IntVarP(&rebuildPort, "port", "p", 8080, "Port to run the application on (defaults to previous port if unspecified)")
}
