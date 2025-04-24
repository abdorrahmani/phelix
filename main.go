package main

import (
	"fmt"
	"github.com/abdorrahmani/gophel/cmd"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"runtime"
)

func main() {
	if err := checkGoInstallation(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	rootCmd := &cobra.Command{
		Use:     "gophel",
		Short:   "Gophel - Go Application Manager",
		Version: cmd.Version,
	}
	rootCmd.AddCommand(cmd.BuildCmd)
	rootCmd.AddCommand(cmd.RebuildCmd)
	rootCmd.AddCommand(cmd.StartCmd)
	rootCmd.AddCommand(cmd.RestartCmd)
	rootCmd.AddCommand(cmd.StatusCmd)
	rootCmd.AddCommand(cmd.StopCmd)
	rootCmd.AddCommand(cmd.ListCmd)
	rootCmd.AddCommand(cmd.LogCmd)
	rootCmd.AddCommand(cmd.AuthCmd)
	rootCmd.AddCommand(cmd.VersionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func checkGoInstallation() error {
	if _, err := exec.LookPath("go"); err != nil {
		fmt.Println("Go not found. Attempting to install the latest version...")
		if runtime.GOOS != "linux" {
			return fmt.Errorf("automatic Go installation is only supported on Linux; please install Go manually")
		}

		cmd := exec.Command("sudo", "apt", "update")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to run apt update: %v", err)
		}
		cmd = exec.Command("sudo", "apt", "install", "-y", "golang-go")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to install Go: %v", err)
		}
		fmt.Println("Go installed successfully!")
	}
	return nil
}
