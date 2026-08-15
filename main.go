package main

import (
	"fmt"
	"log"
	"os"

	"github.com/abdorrahmani/phelix/cmd"
	"github.com/abdorrahmani/phelix/cmd/auth"
	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/spf13/cobra"
)

var healthDaemon *health.GlobalDaemon

func main() {
	err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	healthDaemon = health.InitGlobalDaemon()

	rootCmd := &cobra.Command{
		Use:     "phelix",
		Short:   "Phelix - Go/Rust Application Manager",
		Version: version.Version,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if cmd.Name() == "auth" && len(args) > 0 && args[0] == "status" {
				session, err := auth.GetValidSession()
				if err != nil {
					fmt.Printf("Authentication required: %v\n", err)
					fmt.Println("Please run 'phelix auth login' to authenticate first")
					os.Exit(1)
				}
				if err := auth.VerifySession(session); err != nil {
					fmt.Printf("Invalid session: %v\n", err)
					fmt.Println("Please run 'phelix auth login' again")
					os.Exit(1)
				}
			}
		},
	}

	rootCmd.AddCommand(cmd.BuildCmd)
	rootCmd.AddCommand(cmd.RebuildCmd)
	rootCmd.AddCommand(cmd.RollbackCmd)
	rootCmd.AddCommand(cmd.StartCmd)
	rootCmd.AddCommand(cmd.RestartCmd)
	rootCmd.AddCommand(cmd.StatusCmd)
	rootCmd.AddCommand(cmd.StopCmd)
	rootCmd.AddCommand(cmd.ListCmd)
	rootCmd.AddCommand(cmd.LogCmd)
	rootCmd.AddCommand(auth.Cmd)
	rootCmd.AddCommand(cmd.RemoveCmd)
	rootCmd.AddCommand(cmd.EnvCmd)
	rootCmd.AddCommand(cmd.HealthCmd)
	rootCmd.AddCommand(cmd.ProxyCmd)
	rootCmd.AddCommand(cmd.DockerizeCmd)
	rootCmd.AddCommand(cmd.DeployCmd)
	rootCmd.AddCommand(cmd.MonitorCmd)

	rootCmd.AddCommand(cmd.VersionCmd)
	rootCmd.SetVersionTemplate("Phelix CLI {{.Version}}\n")

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
