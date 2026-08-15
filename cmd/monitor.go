package cmd

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/spf13/cobra"
)

// MonitorCmd starts the long-running gRPC monitoring daemon. It is the process
// systemd supervises directly via `ExecStart=/usr/local/bin/phelix monitor` and
// the process an operator runs in the foreground at a terminal.
var MonitorCmd = &cobra.Command{
	Use:   "monitor",
	Short: "Start the monitoring service",
	Long: `Start the long-running gRPC monitoring daemon.

The daemon:
  - Loads local state and restores any managed applications that were
    previously running (auto-start apps).
  - Establishes a persistent TLS-secured gRPC connection to the Phelix backend.
  - Sends server/app metrics and logs approximately every 2 seconds over a
    single long-lived MonitorStream.
  - Reconnects automatically with exponential backoff if the connection drops.
  - Runs in the foreground (it is what systemd supervises), exiting cleanly on
    SIGTERM/SIGINT.

Use 'sudo systemctl start/stop/restart phelix' to manage the systemd service,
or run this command directly to keep it attached to a terminal.`,
	RunE: func(cobraCmd *cobra.Command, args []string) error {
		return runMonitor()
	},
}

// runMonitor is the daemon entry point. It never returns on its own — it blocks
// until a shutdown signal arrives. Connection failures are handled by the gRPC
// client's reconnect loop; this function only fails fast on unrecoverable
// startup errors (e.g. app state cannot be loaded).
func runMonitor() error {
	if err := app.Manager.LoadState(); err != nil {
		return err
	}
	if _, err := app.Manager.RestoreAutoStartApps(); err != nil {
		// Restoration is best-effort: log the failure and continue so the
		// monitoring connection still comes up. Apps can be started manually.
		log.Printf("[Monitor] App restore had errors: %v", err)
	}

	// The server identity is required for auth metadata and ServerInfo.
	if err := server.Initialize(); err != nil {
		return err
	}

	healthDaemon := health.InitGlobalDaemon()
	phelixgrpc.InitGlobalClient()
	c := phelixgrpc.GetClient()

	// Attach a health reporter BEFORE the client starts so the daemon never
	// misses a health result. The reporter resolves the service client per-send,
	// so it keeps working across reconnects.
	healthDaemon.SetReporter(phelixgrpc.NewGrpcHealthReporter(c))

	// Start the client: connects, and runs the reconnect loop, monitor stream,
	// agent stream and metadata/version syncs in the background.
	c.Start()

	// Start the health-check daemon (auto-restart + endpoint checking). A
	// failure here is not fatal to the monitor — log and continue.
	if err := healthDaemon.Start(); err != nil {
		log.Printf("[Health] Failed to start global daemon: %v", err)
	}

	done := make(chan struct{})

	// Periodic log maintenance (bounds disk usage of the daemon's own log files).
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
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				logs.RemovePreviousLogs()
			}
		}
	}()

	log.Printf("[Monitor] gRPC monitor daemon running")

	// Block until SIGINT/SIGTERM, then shut down cleanly. systemd sends SIGTERM
	// during 'systemctl stop/restart phelix'.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Printf("[Monitor] Shutdown signal received, stopping...")
	close(done)

	// Stop the health-check daemon (flushes/waits its goroutines).
	if err := healthDaemon.Stop(); err != nil {
		log.Printf("[Health] Error stopping daemon: %v", err)
	}

	// Stop sending events and close the gRPC connection + streams. This
	// cancels the monitor/agent streams; their background loops watch the
	// client's done channel and exit. Remaining goroutines end when the process
	// exits through main returning.
	c.Close()

	log.Printf("[Monitor] Daemon stopped")
	return nil
}
