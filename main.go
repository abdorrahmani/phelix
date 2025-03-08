package Gophel

import (
	"fmt"
	"github.com/abdorrahmani/gophel/cmd"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
)

func main() {
	// Check and install Go if not present
	checkGoInstallation()

	rootCmd := &cobra.Command{
		Use:   "gophel",
		Short: "Gophel - Go Application Manager",
	}
	rootCmd.AddCommand(cmd.BuildCmd)
	rootCmd.AddCommand(cmd.RebuildCmd)
	rootCmd.AddCommand(cmd.StartCmd)
	rootCmd.AddCommand(cmd.RestartCmd)
	rootCmd.AddCommand(cmd.StatusCmd)
	rootCmd.AddCommand(cmd.StopCmd)
	rootCmd.AddCommand(cmd.ListCmd)
	rootCmd.AddCommand(cmd.AuthCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func checkGoInstallation() {
	if _, err := exec.LookPath("go"); err != nil {
		fmt.Println("Go not found. Installing latest version...")
		exec.Command("sudo", "apt", "update").Run()
		exec.Command("sudo", "apt", "install", "golang-go").Run()
	}
}
