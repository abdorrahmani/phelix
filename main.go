package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/cmd"
	"github.com/abdorrahmani/phelix/cmd/auth"
	"github.com/abdorrahmani/phelix/config"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/version"
	"github.com/spf13/cobra"
)

var (
	healthDaemon  *health.GlobalDaemon
	done          = make(chan struct{})
	isMonitorMode bool
)

func main() {
	err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	isMonitorMode = len(os.Args) > 1 && os.Args[1] == "monitor"

	healthDaemon = health.InitGlobalDaemon()

	if isMonitorMode {
		grpcClient := phelixgrpc.InitGlobalClient()
		go grpcClient.Start()

		go func() {
			time.Sleep(2 * time.Second)
			if c := phelixgrpc.GetClient(); c != nil && c.IsConnected() {
				reporter := phelixgrpc.NewGrpcHealthReporter(c.GetServiceClient())
				healthDaemon.SetReporter(reporter)
			}
			if err := healthDaemon.Start(); err != nil {
				log.Printf("[Health] Failed to start global daemon: %v", err)
			}
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		if c := phelixgrpc.GetClient(); c != nil {
			c.Close()
		}
		close(done)
		os.Exit(0)
	}()

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

	monitorCmd := &cobra.Command{
		Use:   "monitor",
		Short: "Start the monitoring service",
		Run: func(cobraCmd *cobra.Command, args []string) {

			go func() {
				for {
					select {
					case <-done:
						return
					default:
						logs.RemovePreviousLogs()
						logs.RemoveSelfLogs()
						time.Sleep(1 * time.Minute)
					}
				}
			}()

			go func() {
				tricker := time.NewTicker(15 * time.Minute)
				defer tricker.Stop()
				for {
					select {
					case <-done:
						return
					case <-tricker.C:
						logs.RemovePreviousLogs()
					}
				}
			}()

			log.Printf("[Monitor] gRPC monitor daemon running")

			<-done
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
	rootCmd.AddCommand(monitorCmd)

	rootCmd.AddCommand(cmd.VersionCmd)
	rootCmd.SetVersionTemplate("Phelix CLI {{.Version}}\n")

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
