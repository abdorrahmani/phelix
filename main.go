package main

import (
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
		// config.Load runs before the command tree exists; render through the
		// CLI error boundary so even early failures use the same presentation
		// and exit-code mapping.
		os.Exit(cmd.RenderError(err, cmd.Debug))
	}

	healthDaemon = health.InitGlobalDaemon()

	rootCmd := &cobra.Command{
		Use:     "phelix",
		Short:   "Phelix - Go/Rust Application Manager",
		Version: version.Version,
		// Errors are rendered exactly once by the top-level Execute() handler
		// below. Silence cobra's own printing so nothing is duplicated.
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// `phelix auth status` requires a valid session before the command
			// body runs. Returning an error routes it through the central CLI
			// error renderer (stderr + mapped exit code) instead of printing
			// directly and os.Exit(1).
			if cmd.Name() == "auth" && len(args) > 0 && args[0] == "status" {
				session, err := auth.GetValidSession()
				if err != nil {
					return err
				}
				if _, err := auth.VerifySession(session); err != nil {
					return err
				}
			}
			return nil
		},
	}

	rootCmd.AddCommand(cmd.BuildCmd)
	rootCmd.AddCommand(cmd.RebuildCmd)
	rootCmd.AddCommand(cmd.InitCmd)
	rootCmd.AddCommand(cmd.DoctorCmd)
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
	rootCmd.AddCommand(cmd.UpdateCmd)
	rootCmd.AddCommand(cmd.WizardCmd)
	rootCmd.AddCommand(cmd.BuildReportCmd)
	rootCmd.SetVersionTemplate("Phelix CLI {{.Version}}\n")

	// Global flags: --debug is the single debug switch shared by all
	// subcommands. It is bound to cmd.Debug so the error renderer (which reads
	// the same variable) discloses the root cause chain only in debug mode.
	rootCmd.PersistentFlags().BoolVar(&cmd.Debug, "debug", false, "Show verbose error details and debug output")

	err = rootCmd.Execute()
	if err != nil {
		os.Exit(cmd.RenderError(err, cmd.Debug))
	}
}
