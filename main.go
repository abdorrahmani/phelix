package main

import (
	"context"
	"github.com/abdorrahmani/phelix/internal/app"
	pb "github.com/abdorrahmani/phelix/internal/controlapi/proto"
	"github.com/abdorrahmani/phelix/internal/controller"
	"github.com/abdorrahmani/phelix/internal/server"
	"os"
	"os/signal"
	"syscall"

	"github.com/abdorrahmani/phelix/cmd"
	"github.com/abdorrahmani/phelix/cmd/auth"
	"github.com/abdorrahmani/phelix/config"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/resources"
	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/spf13/cobra"
)

var healthDaemon *health.GlobalDaemon

func main() {
	if resources.RunCleanupHelper() {
		return
	}
	err := config.Load()
	if err != nil {
		// config.Load runs before the command tree exists; render through the
		// CLI error boundary so even early failures use the same presentation
		// and exit-code mapping.
		os.Exit(cmd.RenderError(err, cmd.Debug))
	}

	healthDaemon = health.InitGlobalDaemon()

	rootCmd := &cobra.Command{
		Use:           "phelix",
		Short:         "Phelix - Go/Rust Application Manager",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
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
	rootCmd.AddCommand(cmd.ConfigCmd)
	rootCmd.AddCommand(cmd.DoctorCmd)
	rootCmd.AddCommand(cmd.RollbackCmd)
	rootCmd.AddCommand(cmd.StartCmd)
	rootCmd.AddCommand(cmd.RestartCmd)
	rootCmd.AddCommand(cmd.JobCmd)
	rootCmd.AddCommand(cmd.HooksCmd)
	rootCmd.AddCommand(cmd.ScaleCmd)
	rootCmd.AddCommand(cmd.AutoscaleCmd)
	rootCmd.AddCommand(cmd.StatusCmd)
	rootCmd.AddCommand(cmd.StopCmd)
	rootCmd.AddCommand(cmd.ListCmd)
	rootCmd.AddCommand(cmd.WatchCmd)
	rootCmd.AddCommand(cmd.LogCmd)
	rootCmd.AddCommand(auth.Cmd)
	rootCmd.AddCommand(cmd.RemoveCmd)
	rootCmd.AddCommand(cmd.EnvCmd)
	rootCmd.AddCommand(cmd.SecretsCmd)
	rootCmd.AddCommand(cmd.SecurityCmd)
	rootCmd.AddCommand(cmd.HealthCmd)
	rootCmd.AddCommand(cmd.ProxyCmd)
	rootCmd.AddCommand(cmd.DockerizeCmd)
	rootCmd.AddCommand(cmd.DeployCmd)
	rootCmd.AddCommand(cmd.MonitorCmd)
	rootCmd.AddCommand(cmd.ControllerCmd)
	rootCmd.AddCommand(cmd.WebhookCmd)
	rootCmd.AddCommand(cmd.MatrixCmd)
	rootCmd.AddCommand(cmd.OperationCmd)
	rootCmd.AddCommand(cmd.InspectCmd)
	rootCmd.AddCommand(cmd.PlanCmd)
	rootCmd.AddCommand(cmd.AuthzCmd)
	rootCmd.AddCommand(cmd.ContextCmd)
	rootCmd.AddCommand(cmd.MCPCmd)
	rootCmd.AddCommand(cmd.SessionCmd)

	rootCmd.AddCommand(cmd.VersionCmd)
	rootCmd.AddCommand(cmd.UpdateCmd)
	rootCmd.AddCommand(cmd.WizardCmd)
	rootCmd.AddCommand(cmd.BuildReportCmd)
	rootCmd.AddCommand(cmd.DeploymentReportCmd)
	rootCmd.SetVersionTemplate("Phelix CLI {{.Version}}\n")

	rootCmd.PersistentFlags().BoolVar(&cmd.Debug, "debug", false, "Show verbose error details and debug output")

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	rootCmd.SetContext(ctx)
	if len(os.Args) == 2 && os.Args[1] == controller.AdapterArgument {
		conn, request, readErr := controller.ReadAdapter()
		if readErr != nil {
			os.Exit(cmd.RenderError(readErr, false))
		}
		defer conn.Close()
		if err := os.Chdir(request.Directory); err != nil {
			controller.WriteMessage(conn, &pb.Error{Code: "INVALID_ARGUMENT", Message: "Controller source directory unavailable"})
			return
		}
		id, identityErr := server.EnsureAgentID()
		if identityErr != nil || id != os.Getenv("PHELIX_CONTROLLER_ID") {
			controller.WriteMessage(conn, &pb.Error{Code: "CONFIGURATION", Message: "Controller identity is unavailable or changed"})
			return
		}
		if err := app.Manager.LoadState(); err != nil {
			result, exit := cmd.ControlErrorResult(err, false)
			controller.WriteMessage(conn, result)
			os.Exit(exit)
		}
		rootCmd.SetArgs(request.Argv)
		cmd.SetControlCaller(request.Caller)
		err = rootCmd.Execute()
		result := &pb.Error{}
		exit := 0
		if err != nil {
			result, exit = cmd.ControlErrorResult(err, cmd.Debug)
		}
		controller.WriteMessage(conn, result)
		conn.Close()
		os.Exit(exit)
	}
	cmd.RouteControl(rootCmd)
	cmd.ConfigureWizards(rootCmd)
	err = rootCmd.Execute()
	if err != nil {
		os.Exit(cmd.RenderError(err, cmd.Debug))
	}
}
