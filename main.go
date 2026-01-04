package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/abdorrahmani/gophel/cmd"
	"github.com/abdorrahmani/gophel/cmd/auth"
	"github.com/abdorrahmani/gophel/config"
	"github.com/abdorrahmani/gophel/internal/logs"
	"github.com/abdorrahmani/gophel/internal/monitor"
	"github.com/spf13/cobra"
)

var (
	monitorService monitor.MonitorService
	done           = make(chan struct{})
	isMonitorMode  bool
)

func main() {
	err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// Check if running in monitor mode
	isMonitorMode = len(os.Args) > 1 && os.Args[1] == "monitor"

	// Skip Go installation check for monitor command
	if !isMonitorMode {
		if err := checkGoInstallation(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	}

	// Initialize monitor service
	monitorService = monitor.NewMonitorService()

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		close(done)
		os.Exit(0)
	}()

	rootCmd := &cobra.Command{
		Use:     "gophel",
		Short:   "Gophel - Go Application Manager",
		Version: cmd.Version,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Only check authentication for auth status command
			if cmd.Name() == "auth" && len(args) > 0 && args[0] == "status" {
				session, err := auth.GetValidSession()
				if err != nil {
					fmt.Printf("Authentication required: %v\n", err)
					fmt.Println("Please run 'gophel auth login' to authenticate first")
					os.Exit(1)
				}
				if err := auth.VerifySession(session); err != nil {
					fmt.Printf("Invalid session: %v\n", err)
					fmt.Println("Please run 'gophel auth login' again")
					os.Exit(1)
				}
			}
		},
	}

	// Add monitor command
	monitorCmd := &cobra.Command{
		Use:   "monitor",
		Short: "Start the WebSocket monitoring service",
		Run: func(cobraCmd *cobra.Command, args []string) {

			go func() {
				for {
					select {
					case <-done:
						return
					default:
						logs.RemovePreviousLogs()
						logs.RemoveSelfLogs()
						time.Sleep(1 * time.Minute) // each minute
					}
				}
			}()

			// For delete each 15 minute fil logs
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

			// Start WebSocket monitoring
			go func() {
				for {
					select {
					case <-done:
						return
					default:
						session, err := auth.GetValidSession()
						if err != nil {
							log.Printf("[WebSocket] No valid session found: %v", err)
							time.Sleep(5 * time.Second)
							continue
						}

						log.Printf("[WebSocket] Starting monitoring with session ID: %s", session.SessionID)
						if err := monitorService.StartMonitoring(); err != nil {
							log.Printf("[WebSocket] Error starting monitoring: %v", err)
							time.Sleep(5 * time.Second)
							continue
						}

						// Keep the goroutine running
						time.Sleep(24 * time.Hour)
					}
				}
			}()

			// Keep the main process running
			<-done
		},
	}

	// Add commands
	rootCmd.AddCommand(cmd.BuildCmd)
	rootCmd.AddCommand(cmd.RebuildCmd)
	rootCmd.AddCommand(cmd.StartCmd)
	rootCmd.AddCommand(cmd.RestartCmd)
	rootCmd.AddCommand(cmd.StatusCmd)
	rootCmd.AddCommand(cmd.StopCmd)
	rootCmd.AddCommand(cmd.ListCmd)
	rootCmd.AddCommand(cmd.LogCmd)
	rootCmd.AddCommand(auth.Cmd)
	rootCmd.AddCommand(cmd.VersionCmd)
	rootCmd.AddCommand(cmd.RemoveCmd)
	rootCmd.AddCommand(monitorCmd)

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
