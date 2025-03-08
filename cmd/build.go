package cmd

import (
	"fmt"
	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
	"os/exec"
)

var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Builds and runs a Go application",
	Long:  "Compiles a Go application from the current directory and starts it immediately",
	Run: func(cmd *cobra.Command, args []string) {
		id := app.GenerateAppID()
		fmt.Printf("Building application, ID: %s\n", id)

		// Build the Go application
		if err := exec.Command("go", "build", "-o", fmt.Sprintf("app_%s", id)).Run(); err != nil {
			fmt.Println("Build failed:", err)
			return
		}

		// Run the application
		app.StartApplication(id)
	},
}
