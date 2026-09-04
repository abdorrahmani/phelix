package cmd

import (
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

	// Apps managed by a zero-downtime deployment must never be classic-started
	// on the public port; they are restored below through their deployment
	// (active slot relaunch + proxy route restore).
	app.DeployedAppSkipper = func(id string) bool {
		if info, err := GetAppInfo(id); err == nil {
			return loadDeployState(info.Name) != nil
		}
		return false
	}
	// Same rule for the health daemon's auto-restart: tear the deployment down
	// and bring it back, exactly as `phelix restart` does, instead of killing
	// the serving instance and rebinding the proxy-owned public port.
	app.DeployedAppRestarter = func(id string) error {
		info, err := GetAppInfo(id)
		if err != nil {
			return err
		}
		if err := runDeployAwareStop(info); err != nil {
			return err
		}
		return runDeployAwareStart(info)
	}
	restoreDeployedApps()

	if _, err := app.Manager.RestoreAutoStartApps(); err != nil {
		// Restoration is best-effort: log the failure and continue so the
		// monitoring connection still comes up. Apps can be started manually.
		logs.Warning("monitor", "app restore had errors: %v", err)
	}

	// The server identity is required for auth metadata and ServerInfo. The
	// agent_id in the startup log ties every daemon log line to one runtime and
	// makes the identity visible across hosts sharing a machine-id.
	if err := server.Initialize(); err != nil {
		return err
	}
	logs.Info("monitor", "loaded agent identity agent_id=%s", server.GetAgentID())

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
		logs.Warning("monitor", "failed to start health daemon: %v", err)
	}

	done := make(chan struct{})

	// Periodic log maintenance (bounds disk usage of the daemon's own log files).
	appTargets := func() []logs.AppLogTarget {
		var targets []logs.AppLogTarget
		for _, a := range app.Manager.ListApplications() {
			targets = append(targets, logs.AppLogTarget{ID: a.ID})
		}
		return targets
	}

	go func() {
		for {
			select {
			case <-done:
				return
			default:
				logs.RemovePreviousLogs(appTargets())
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
				logs.RemovePreviousLogs(appTargets())
			}
		}
	}()

	// Block until SIGINT/SIGTERM, then shut down cleanly. systemd sends SIGTERM
	// during 'systemctl stop/restart phelix'.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logs.Info("monitor", "shutdown signal received, stopping...")
	close(done)

	// Stop the health-check daemon (flushes/waits its goroutines).
	if err := healthDaemon.Stop(); err != nil {
		logs.Warning("monitor", "error stopping health daemon: %v", err)
	}

	// Stop sending events and close the gRPC connection + streams. This
	// cancels the monitor/agent streams; their background loops watch the
	// client's done channel and exit. Remaining goroutines end when the process
	// exits through main returning.
	c.Close()

	logs.Info("monitor", "daemon stopped")
	return nil
}

// restoreDeployedApps best-effort restores zero-downtime deployments the way
// `phelix start` would: proxy daemon up, active slot relaunched from its
// recorded binary, proxy route re-established. Failures are logged, never
// fatal — the operator can always run `phelix start <app>` by hand.
func restoreDeployedApps() {
	am, ok := app.Manager.(*app.AppManager)
	if !ok {
		return
	}
	for _, info := range am.Apps {
		state := loadDeployState(info.Name)
		if state == nil {
			continue
		}
		// Only restore apps that were serving before (auto-start intent).
		if !info.AutoStart || info.Status == "running" {
			continue
		}
		if err := runDeployAwareStart(info); err != nil {
			logs.Warning("monitor", "failed to restore deployment for %s: %v", info.Name, err)
		}
	}
}
