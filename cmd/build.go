package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
	"os/exec"
)

var port int

var BuildCmd = &cobra.Command{
	Use:   "build <NAME> --port <PORT>",
	Short: "Builds and runs a Go application with a specified name",
	Long:  "Compiles a Go application from the current directory with the given name and starts it immediately",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		name := args[0]
		id := app.Manager.GenerateAppID()
		fmt.Printf("Building application '%s', ID: %s\n", name, id)

		if err := app.Manager.LoadState(); err != nil {
			fmt.Printf("Failed to load state: %v\n", err)
			return
		}
		for _, app := range app.Manager.(*app.AppManager).Apps {
			if app.Name == name {
				fmt.Printf("Application name '%s' is already in use\n", name)
				return
			}
		}

		// Build the Go application
		if err := exec.Command("go", "build", "-o", fmt.Sprintf("app_%s", id)).Run(); err != nil {
			fmt.Println("Build failed:", err)
			return
		}

		// Run the application
		app.Manager.StartApplication(id, port, name)
	},
}

func init() {
	BuildCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the application on")
}
