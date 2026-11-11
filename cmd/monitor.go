package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/autoscale"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	pb "github.com/abdorrahmani/phelix/internal/grpc/proto"
	"github.com/abdorrahmani/phelix/internal/health"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/monitor"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/supervisor"
	"github.com/abdorrahmani/phelix/internal/syscmd"
	"github.com/abdorrahmani/phelix/internal/update"
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

// runSecurityScheduler drives scheduled dependency vulnerability scans for
// apps whose phelix.yaml enables them. Errors and completions are logged
// without ever emitting dependency inventory details.
func runSecurityScheduler(ctx context.Context) {
	scheduler := security.NewScheduler(security.NewScanStore(server.DataDir()))
	scheduler.ListApps = listSecurityScheduledApps
	scheduler.Scan = func(scanCtx context.Context, sched security.ScheduledApp) error {
		opts := security.ScanOptions{
			AppID:       sched.AppID,
			AppName:     sched.AppName,
			IdentityKey: sched.IdentityKey,
			Dir:         sched.Dir,
			Trigger:     security.TriggerScheduled,
			DataDir:     server.DataDir(),
			OSVBaseURL:  security.OSVBaseURL(),
		}
		_, err := security.RunScan(scanCtx, opts)
		return err
	}
	scheduler.Log = func(level, format string, args ...any) {
		if level == "warning" {
			logs.Warning("security", format, args...)
			return
		}
		logs.Info("security", format, args...)
	}
	scheduler.Run(ctx)
}

// remoteSecurityAuthorize gates remote security mutations through the
// host authorization boundary. The application identifier resolves to its
// stable id before the decision so policies can target apps precisely.
func remoteSecurityAuthorize(req *pb.MonitorCommandRequest) error {
	target, err := remoteSecurityTarget(req.GetAppName())
	if err != nil {
		return err
	}
	return authorizeDirectExecution(authz.ActionSecurity, target.appName, target.appID)
}

// securityProjectSkips counts applications whose project configuration
// could not be read during one periodic pass. Aggregating the count keeps a
// large registry with stale project directories from producing one log line
// per application per tick.
type securityProjectSkips struct {
	unavailable int
	unreadable  int
}

// add classifies one failed project configuration read.
func (s *securityProjectSkips) add(err error) {
	if phelixerr.CodeOf(err) == phelixerr.CodeSecurityDirectoryUnavailable {
		s.unavailable++
		return
	}
	s.unreadable++
}

// any reports whether the pass skipped at least one application.
func (s securityProjectSkips) any() bool {
	return s.unavailable > 0 || s.unreadable > 0
}

// log writes one summary warning for the pass.
func (s securityProjectSkips) log(action string) {
	if !s.any() {
		return
	}
	securityWarningFile("security", "%s: skipped %d application(s) with an unavailable project directory and %d with unreadable security configuration", action, s.unavailable, s.unreadable)
}

// securityWarningFile is the warning sink for periodic security passes. It is
// a variable so tests can assert that a pass emits one summary line instead
// of one line per application.
var securityWarningFile = logs.WarningFile

// securitySnapshotsForConsentingApps builds the latest completed scan
// summary for every application that opted in to security reporting.
func securitySnapshotsForConsentingApps() []*pb.SecurityScanSnapshot {
	var out []*pb.SecurityScanSnapshot
	var skips securityProjectSkips
	for _, info := range app.Manager.ListApplications() {
		cfg, err := project.LoadSecurityProject(info.Directory)
		if err != nil {
			skips.add(err)
			continue
		}
		if remoteSecurityConsent(&securityTarget{cfg: cfg}) != nil {
			continue
		}
		store := security.NewScanStore(server.DataDir())
		entry, err := store.Latest(info.ID)
		if err != nil || entry == nil || entry.ScanID == "" {
			continue
		}
		record, err := store.LoadScan(entry.ScanID)
		if err != nil || record.Status != security.ScanCompleted {
			continue
		}
		snapshot := &pb.SecurityScanSnapshot{
			ScanId:    record.ScanID,
			AppId:     info.ID,
			AppName:   info.Name,
			Status:    string(record.Status),
			Freshness: record.Freshness,
			Coverage:  record.Coverage,
			Counts: &pb.SecurityScanCounts{
				Modules:             int32(record.Counts.Modules),
				Matchable:           int32(record.Counts.Matchable),
				Evaluated:           int32(record.Counts.Evaluated),
				Affected:            int32(record.Counts.Affected),
				PotentiallyAffected: int32(record.Counts.PotentiallyAffected),
				Unknown:             int32(record.Counts.Unknown),
				NotAffected:         int32(record.Counts.NotAffected),
			},
		}
		if record.FinishedAt != nil {
			snapshot.FinishedAtMs = record.FinishedAt.UnixMilli()
		}
		out = append(out, snapshot)
	}
	skips.log("cannot resolve reporting configuration")
	return out
}

// listSecurityScheduledApps reports every registered application with its
// scan settings. Apps whose phelix.yaml is missing or silent are reported
// disabled rather than guessed.
func listSecurityScheduledApps() []security.ScheduledApp {
	apps := app.Manager.ListApplications()
	out := make([]security.ScheduledApp, 0, len(apps))
	var skips securityProjectSkips
	for _, info := range apps {
		entry := security.ScheduledApp{
			AppID:       info.ID,
			AppName:     info.Name,
			IdentityKey: info.ID,
			Dir:         info.Directory,
			Interval:    project.ScanIntervalDefault,
		}
		cfg, err := project.LoadSecurityProject(info.Directory)
		if err != nil {
			skips.add(err)
			continue
		}
		if cfg.Security != nil && cfg.Security.Vulnerability != nil {
			v := cfg.Security.Vulnerability
			entry.Enabled = v.Enabled
			if interval, err := v.ResolvedScanInterval(); err == nil {
				entry.Interval = interval
			}
		}
		out = append(out, entry)
	}
	skips.log("cannot load scan configuration")
	return out
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
			if w, werr := workerAppState(info.Name); werr == nil && w != nil {
				return true
			}
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
		return restartAppInfo(info)
	}
	app.RestoreAppGuard = func(id, name string) (func(), error) {
		release, err := deploy.AcquireDeployLock(name, "restore-classic")
		if err != nil {
			return nil, err
		}
		if _, err := deploy.ReconcileSecretTxn(name, id); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
	startupSecretsRuntime()
	restoreDeployedApps()
	recoverInterruptedScales()

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
	if err := phelixgrpc.InitializeWorkerLedger(); err != nil {
		return err
	}
	phelixgrpc.SetWorkerHandler(RemoteWorkerCommand)
	phelixgrpc.SetWorkerAuthorizer(remoteWorkerAuthorize)
	if err := phelixgrpc.InitializeAutoscaleLedger(); err != nil {
		return err
	}
	phelixgrpc.SetAutoscaleHandler(RemoteAutoscaleCommand)
	phelixgrpc.SetAutoscaleAuthorizer(remoteAutoscaleAuthorize)
	if err := phelixgrpc.InitializeJobLedger(); err != nil {
		return err
	}
	phelixgrpc.SetJobHandler(RemoteJobCommand)
	phelixgrpc.SetJobAuthorizer(remoteJobAuthorize)

	supervisorCtx, cancelSupervisor := context.WithCancel(context.Background())
	defer cancelSupervisor()
	go func() {
		logs.Info("monitor", "worker supervisor loop started")
		supervisor.New().Run(supervisorCtx)
	}()

	autoscaleCtx, cancelAutoscale := context.WithCancel(context.Background())
	defer cancelAutoscale()
	autoscaleDone := make(chan struct{})
	go func() {
		defer close(autoscaleDone)
		controller := newAutoscaleController()
		if controller == nil {
			return
		}
		logs.Info("monitor", "autoscaling controller started")
		controller.Run(autoscaleCtx)
	}()

	securityCtx, cancelSecurity := context.WithCancel(context.Background())
	defer cancelSecurity()
	securityDone := make(chan struct{})
	go func() {
		defer close(securityDone)
		runSecurityScheduler(securityCtx)
	}()

	jobCtx, cancelJobs := context.WithCancel(context.Background())
	defer cancelJobs()
	jobsDone := make(chan struct{})
	go func() {
		defer close(jobsDone)
		runJobScheduler(jobCtx)
	}()

	monitor.SetRollbackHandler(func(payload monitor.CommandPayload) error {
		return RemoteRollback(payload)
	})

	monitor.SetRestartAgentHandler(func() error {
		return (update.Systemd{Runner: syscmd.ExecRunner{}}).RequestRestart()
	})

	matrixCtx, cancelMatrixSessions := context.WithCancel(context.Background())
	defer cancelMatrixSessions()
	phelixgrpc.SetMatrixHandler(func(req *pb.MonitorCommandRequest) *pb.MonitorCommandResult {
		return RemoteMatrixCommand(matrixCtx, req)
	})

	phelixgrpc.SetMatrixAuthorizer(remoteMatrixAuthorize)

	phelixgrpc.SetWebhookHandler(RemoteWebhookCommand)

	if err := phelixgrpc.InitializeSecurityLedger(); err != nil {
		logs.Warning("monitor", "remote security ledger unavailable; mutating security commands will fail closed: %v", err)
	}
	phelixgrpc.SetSecurityHandler(RemoteSecurityCommand)
	phelixgrpc.SetSecurityAuthorizer(remoteSecurityAuthorize)
	phelixgrpc.SecuritySnapshotProvider = securitySnapshotsForConsentingApps

	c.Start()

	if err := healthDaemon.Start(); err != nil {
		logs.Warning("monitor", "failed to start health daemon: %v", err)
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				for _, info := range app.Manager.ListApplications() {
					_ = deploy.RefreshSecretCoverage(info)
				}
			}
		}
	}()

	appTargets := func() []logs.AppLogTarget {
		var targets []logs.AppLogTarget
		for _, a := range app.Manager.ListApplications() {
			targets = append(targets, monitor.AppLogTargets(a)...)
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

	cancelSecurity()
	select {
	case <-securityDone:
	case <-time.After(10 * time.Second):
		logs.Warning("security", "scheduler did not settle within the shutdown window")
	}

	cancelJobs()
	select {
	case <-jobsDone:
	case <-time.After(10 * time.Second):
		logs.Warning("job", "scheduler did not settle within the shutdown window")
	}

	cancelMatrixSessions()
	phelixgrpc.WaitPendingMatrixCommands(phelixgrpc.MatrixCommandSettleTimeout())
	phelixgrpc.WaitPendingWebhookCommands(phelixgrpc.WebhookCommandSettleTimeout())
	phelixgrpc.WaitPendingWorkerCommands(phelixgrpc.WorkerCommandSettleTimeout())
	phelixgrpc.WaitPendingAutoscaleCommands(phelixgrpc.AutoscaleCommandSettleTimeout())
	phelixgrpc.WaitPendingJobCommands(phelixgrpc.JobCommandSettleTimeout())
	phelixgrpc.StopMatrixSender(5 * time.Second)

	cancelAutoscale()
	select {
	case <-autoscaleDone:
	case <-time.After(5 * time.Second):
		logs.Warning("monitor", "autoscaling controller did not stop within the shutdown window")
	}

	close(done)

	if err := healthDaemon.Stop(); err != nil {
		logs.Warning("monitor", "error stopping health daemon: %v", err)
	}

	if err := phelixgrpc.NotifyDisconnect(); err != nil {
		logs.Warning("monitor", "could not notify backend of shutdown: %v", err)
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
	restoreWorkerApps(am)
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
		if !info.AutoStart || deploy.DecideLifecycle(state).Status == "running" {
			continue
		}
		if err := runDeployAwareStart(info); err != nil {
			logs.Warning("monitor", "failed to restore deployment for %s: %v", info.Name, err)
		}
	}
	reconcileOrphanContainers(am)
}

// restoreWorkerApps heals worker apps at daemon start: interrupted rollouts
// are reverted and orphaned processes reported. Replicas that died while the
// daemon was down are deliberately left for the supervisor loop, which
// applies the restart policy with the persisted counters intact; a blanket
// WorkerStart here would silently reset crash-loop state.
func restoreWorkerApps(am *app.AppManager) {
	for _, info := range am.Apps {
		wstate, werr := workerAppState(info.Name)
		if werr != nil || wstate == nil {
			continue
		}
		if rerr := deploy.RecoverWorkerRollout(context.Background(), info.Name); rerr != nil {
			logs.Warning("monitor", "worker %s: rollout recovery failed: %v", info.Name, rerr)
		}
		wstate, werr = workerAppState(info.Name)
		if werr != nil || wstate == nil {
			continue
		}
		if pids := deploy.DetectWorkerOrphans(wstate); len(pids) > 0 {
			logs.Warning("monitor", "worker %s: %d orphaned replica process(es) running outside the deploy state: %v", info.Name, len(pids), pids)
		}
	}
}

// reconcileOrphanContainers reclaims phelix.managed containers that no app's
// deploy state accounts for — the debris a deploy leaves when it crashes after
// `docker run` but before persisting the new instance. It runs after the
// deploy-aware restores above so any container a restore just (re)created is in
// the allow-list. It is a no-op on a pure-native host: with no docker instances
// recorded, the allow-list is empty and the `docker ps` filter matches nothing
// (or docker is absent and the listing errors — logged, not fatal).
func reconcileOrphanContainers(am *app.AppManager) {
	for _, info := range am.Apps {
		release, err := deploy.AcquireDeployLock(info.Name, "orphan-reconcile")
		if err != nil {
			continue
		}
		state, err := loadDeployState(info.Name)
		if err != nil {
			release()
			logs.Warning("monitor", "cannot read deployment state for %s, skipping orphan check: %v", info.Name, err)
			continue
		}
		if state == nil || state.Runtime != deploy.RuntimeDocker {
			release()
			continue
		}
		known := map[string]bool{}
		for _, id := range deploy.KnownContainerIDs(state) {
			known[id] = true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		removed, err := deploy.ReconcileOrphanContainers(ctx, info.Name, known)
		cancel()
		release()
		if err != nil {
			logs.Warning("monitor", "orphan container check for %s failed: %v", info.Name, err)
		} else if len(removed) > 0 {
			logs.Info("monitor", "reclaimed %d orphaned containers for %s", len(removed), info.Name)
		}
	}
}

// recoverInterruptedScales converges automatic scaling operations that a
// monitor crash left in flight: each rolling app's deploy.json scale marker
// is reconciled against live processes and proxy membership before the
// controller starts. Failures are logged, never fatal.
func recoverInterruptedScales() {
	var proxyClient *proxy.Client
	if socket, err := proxy.DefaultSocketPath(); err == nil {
		proxyClient = proxy.NewClient(socket)
	}
	for _, info := range app.Manager.ListApplications() {
		state, err := loadDeployState(info.Name)
		if err != nil || state == nil || state.Mode != deploy.ModeRolling || state.ScaleProgress == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if err := deploy.RecoverInterruptedScale(ctx, info.Name, proxyClient); err != nil {
			logs.Warning("monitor", "autoscale recovery for %s failed: %v", info.Name, err)
		} else {
			logs.Info("monitor", "autoscale recovery for %s converged", info.Name)
		}
		cancel()
	}
}

// newAutoscaleController builds the autoscaling controller over the app
// registry and the proxy control socket. A nil proxy client is valid: apps
// then scale on resource metrics only, with traffic reported not applicable.
func newAutoscaleController() *autoscale.Controller {
	var proxyClient *proxy.Client
	if socket, err := proxy.DefaultSocketPath(); err == nil {
		proxyClient = proxy.NewClient(socket)
	}
	apps := func() []autoscale.AppRef {
		refs := make([]autoscale.AppRef, 0, 8)
		for _, info := range app.Manager.ListApplications() {
			refs = append(refs, autoscale.AppRef{Name: info.Name, ID: info.ID, Directory: info.Directory})
		}
		return refs
	}
	return autoscale.NewController(apps, proxyClient)
}
