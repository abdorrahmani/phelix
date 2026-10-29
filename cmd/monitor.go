package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/monitor"
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

	// Local profiling server, started before everything else so startup is
	// profileable too. Disabled unless PHELIX_PPROF holds a bind address;
	// loopback by default (see pprof.go). The returned shutdown func is nil
	// when profiling is disabled, so guard the deferred call.
	if stopPprof := startPprofIfEnabled(); stopPprof != nil {
		defer stopPprof()
	}

	if err := app.Manager.LoadState(); err != nil {
		return err
	}

	app.DeployedAppSkipper = func(id string) bool {
		if info, err := GetAppInfo(id); err == nil {
			state, derr := loadDeployState(info.Name)
			if derr != nil {
				logs.Warning("monitor", "cannot read deployment state for %s, treating as deploy-managed: %v", info.Name, derr)
				return true
			}
			return state != nil
		}
		return false
	}

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

		logs.Warning("monitor", "app restore had errors: %v", err)
	}

	if err := server.Initialize(); err != nil {
		return err
	}
	logs.Info("monitor", "loaded agent identity agent_id=%s", server.GetAgentID())

	if scope := phelixgrpc.PreferredSessionScope(); scope != phelixgrpc.SessionScopeAgent {
		logs.Warning("monitor",
			"daemon is authenticating with a full-scope session token; for least privilege run 'phelix auth login --scope agent' on this machine (monitoring-only token, see README § Authentication)")
	}

	healthDaemon := health.InitGlobalDaemon()
	phelixgrpc.InitGlobalClient()
	c := phelixgrpc.GetClient()

	healthDaemon.SetReporter(phelixgrpc.NewGrpcHealthReporter(c))

	if err := phelixgrpc.InitializeRollbackLedger(); err != nil {
		return err
	}
	if err := phelixgrpc.InitializeMatrixLedger(); err != nil {
		return err
	}
	if err := phelixgrpc.InitializeWebhookLedger(); err != nil {
		return err
	}

	monitor.SetRollbackHandler(func(payload monitor.CommandPayload) error {
		return RemoteRollback(payload)
	})

	matrixCtx, cancelMatrixSessions := context.WithCancel(context.Background())
	defer cancelMatrixSessions()
	phelixgrpc.SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		return RemoteMatrixCommand(matrixCtx, req)
	})

	phelixgrpc.SetMatrixAuthorizer(remoteMatrixAuthorize)

	phelixgrpc.SetWebhookHandler(RemoteWebhookCommand)

	c.Start()

	if err := healthDaemon.Start(); err != nil {
		logs.Warning("monitor", "failed to start health daemon: %v", err)
	}

	done := make(chan struct{})

	appTargets := func() []logs.AppLogTarget {
		var targets []logs.AppLogTarget
		for _, a := range app.Manager.ListApplications() {
			targets = append(targets, a.LogTarget())
		}
		return targets
	}

	go func() {
		for {
			select {
			case <-done:
				return
			default:
				logs.RotateAppLogs(appTargets())
				logs.RotateSelfLogs()
				time.Sleep(1 * time.Minute)
			}
		}
	}()

	if logs.LogStoreEnabled() {
		go logs.StartStoreRetention(done)
	}

	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				logs.RotateAppLogs(appTargets())
			}
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logs.Info("monitor", "shutdown signal received, stopping...")

	cancelMatrixSessions()
	phelixgrpc.WaitPendingMatrixCommands(phelixgrpc.MatrixCommandSettleTimeout())
	phelixgrpc.WaitPendingWebhookCommands(phelixgrpc.WebhookCommandSettleTimeout())
	phelixgrpc.StopMatrixSender(5 * time.Second)

	close(done)

	if err := healthDaemon.Stop(); err != nil {
		logs.Warning("monitor", "error stopping health daemon: %v", err)
	}

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
		state, derr := loadDeployState(info.Name)
		if derr != nil {
			logs.Warning("monitor", "cannot read deployment state for %s: %v", info.Name, derr)
			continue
		}
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
	reconcileOrphanContainers(am)
}

// reconcileOrphanContainers reclaims phelix.managed containers that no app's
// deploy state accounts for — the debris a deploy leaves when it crashes after
// `docker run` but before persisting the new instance. It runs after the
// deploy-aware restores above so any container a restore just (re)created is in
// the allow-list. It is a no-op on a pure-native host: with no docker instances
// recorded, the allow-list is empty and the `docker ps` filter matches nothing
// (or docker is absent and the listing errors — logged, not fatal).
func reconcileOrphanContainers(am *app.AppManager) {
	known := map[string]bool{}
	anyDocker := false
	for _, info := range am.Apps {
		state, derr := loadDeployState(info.Name)
		if derr != nil {
			logs.Warning("monitor", "cannot read deployment state for %s, skipping orphan check: %v", info.Name, derr)
			continue
		}
		if state == nil {
			continue
		}
		if state.Runtime == deploy.RuntimeDocker {
			anyDocker = true
		}
		for _, id := range deploy.KnownContainerIDs(state) {
			known[id] = true
		}
	}

	if !anyDocker && len(known) == 0 {
		return
	}
	removed, err := deploy.ReconcileOrphanContainers(context.Background(), known)
	if err != nil {
		logs.Warning("monitor", "orphan container reconcile skipped: %v", err)
		return
	}
	if len(removed) > 0 {
		logs.Info("monitor", "reclaimed %d orphaned phelix container(s) left by an interrupted deploy", len(removed))
	}
}
