package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/ops"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var rebuildPort int
var rebuildArgs []string
var rebuildBlueGreen bool
var rebuildReplicas int
var rebuildStrategy string
var rebuildTag string
var rebuildAutoRollback bool
var rebuildCanary int
var rebuildSourceDir string
var rebuildJSON bool
var rebuildRequestKey string
var rebuildTimeout time.Duration

// rolloutPlan is non-nil when this rebuild takes the canary/progressive path.
// It is resolved by applyConfigDeployStrategy from --canary, --strategy or
// phelix.yaml before the zero-downtime branch dispatches on it.
var rolloutPlan *deploy.RolloutPlan

var RebuildCmd = &cobra.Command{
	Use:           "rebuild [ID|AppName] --port <PORT>",
	Short:         "Rebuilds and runs a Go Application by its ID or AppName.",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) (err error) {
		if rebuildJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		spec, specErr := buildRebuildSpec(cmd, args)
		if specErr != nil {
			return specErr
		}

		if authzErr := authorizeDirectExecution(authz.ActionRebuild, spec.Name, spec.AppInfo.ID); authzErr != nil {
			return authzErr
		}

		op := newOpRun(ops.KindRebuild, spec.Name, rebuildRequestKey)
		defer func() { op.finish(&err) }()
		replayed, idemErr := op.beginIdempotency(map[string]string{
			"strategy":   spec.Strategy,
			"replicas":   fmt.Sprintf("%d", spec.Replicas),
			"canary":     fmt.Sprintf("%d", spec.Canary),
			"port":       fmt.Sprintf("%d", spec.Port),
			"source_dir": rebuildSourceDir,
			"tag":        rebuildTag,
		})
		if idemErr != nil {
			return idemErr
		}
		if replayed {
			return nil
		}

		return runRebuildExec(spec, op)
	},
}

// runClassicRebuild executes the classic stop -> build -> start -> promote
// path for a resolved rebuild spec. It is the tail of the original rebuild
// command body, unchanged except for taking the spec as its input.
func runClassicRebuild(spec *rebuildSpec, op *opRun) error {
	appInfo, name, portToUse, buildSource := spec.AppInfo, spec.Name, spec.Port, spec.SourceDir
	buildMgr := spec.BuildMgr

	release, lockErr := deploy.AcquireDeployLock(name, "rebuild")
	if lockErr != nil {
		return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", lockErr)
	}
	defer release()

	currentVer, _ := deploy.CurrentVersion(name)
	tracker, flushTelemetry := classicTracker(appInfo.ID, name, portToUse, currentVer)
	defer flushTelemetry()
	op.setDeploymentID(tracker)

	if err := stopExistingApp(appInfo); err != nil {
		tracker.Failed(err)
		return err
	}

	tracker.Building("classic rebuild")
	rebuildReport, rerr := rebuildApp(appInfo.ID, rebuildArgs, buildMgr, buildSource)
	if rerr != nil {
		tracker.Failed(rerr)
		return rerr
	}

	gitCommit := deploy.DetectGitCommit(buildSource)
	logger := &colorLogger{}
	rec, verErr := deploy.RecordFreshBuild(
		name, appInfo.ID,
		filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", appInfo.ID)),
		gitCommit, rebuildTag,
		rebuildReport,
		deploy.DefaultRetention{Max: 5}, logger,
	)
	if verErr != nil {
		fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
	} else {
		fmt.Printf("  %s Recorded version v%d\n", color.BlueString("→"), rec.Version)
		tracker.SetTargetVersion(rec.Version)
	}

	emitBuildReport(name, rebuildReport, gitCommit, rec, verErr)

	fmt.Printf("  %s Starting application on port %d...\n", color.BlueString("→"), portToUse)
	if err := app.Manager.StartApplication(appInfo.ID, portToUse, name); err != nil {

		startErr := phelixerr.Wrapf(
			phelixerr.CodeProcessFailed,
			err,
			"failed to start rebuilt application %q (ID: %s)",
			name,
			appInfo.ID,
		)
		tracker.Failed(startErr)
		if rec != nil && rebuildAutoRollback {
			runClassicAutoRollback(name, appInfo, rec.Version, portToUse, startErr)
		}
		return startErr
	}
	tracker.InstanceStarted("", classicPID(appInfo.ID), portToUse)

	if rec != nil {
		if err := deploy.PromoteVersion(name, rec.Version, "classic"); err != nil {
			fmt.Printf("  %s Warning: could not promote version: %v\n", color.YellowString("⚠"), err)
		}
	}
	tracker.PromoteCurrentVersion()
	tracker.Completed(fmt.Sprintf("running on port %d", portToUse))

	fmt.Printf("%s Application %s (ID: %s) rebuilt and started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), portToUse)
	phelixgrpc.ReportEvent(appInfo.ID, name, "rebuild", true, "", 0, "", "")
	phelixgrpc.SendVersionListForApp(appInfo.ID, name, appInfo.Directory)
	rbVer := 0
	if rec != nil {
		rbVer = rec.Version
	}
	phelixgrpc.ReportBuildEventForApp(appInfo.ID, name, "classic", true, rbVer, rebuildTag, gitCommit, rebuildReport, "")

	op.setResult(&ops.Result{Version: rbVer, Port: portToUse, Strategy: "classic"})
	return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{
		App:      name,
		AppID:    appInfo.ID,
		Version:  rbVer,
		Port:     portToUse,
		Strategy: "classic",
	}))
}

// applyConfigDeployStrategy resolves which deployment path this rebuild takes
// and maps it onto the existing --blue-green / --replicas flags or a rollout
// plan.
func applyConfigDeployStrategy(cmd *cobra.Command, cfg *project.Config, appName string) error {
	if cmd.Flags().Changed("canary") {
		if cmd.Flags().Changed("blue-green") || cmd.Flags().Changed("replicas") {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"--canary cannot be combined with --blue-green or --replicas")
		}
		if rebuildStrategy != "" {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"--canary cannot be combined with --strategy (the flag already selects a canary rollout)")
		}
		if rebuildCanary < 1 || rebuildCanary > 99 {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"--canary must be between 1 and 99 (percent of traffic for the canary), got %d", rebuildCanary)
		}
		plan, err := canaryPlanFromFlag(cfg, rebuildCanary)
		if err != nil {
			return err
		}
		rolloutPlan = plan
		return nil
	}

	if cmd.Flags().Changed("blue-green") || cmd.Flags().Changed("replicas") {
		return nil
	}

	strategy := rebuildStrategy
	if strategy == "" && cfg != nil && cfg.Deploy != nil {
		strategy = cfg.Deploy.Strategy
	}
	deployed, derr := loadDeployState(appName)
	if derr != nil {
		return derr
	}
	if strategy == "" && deployed != nil {
		strategy = string(deployed.Mode)
	}

	switch strategy {
	case "", project.StrategyClassic:
	case project.StrategyBlueGreen:
		rebuildBlueGreen = true
	case project.StrategyRolling:
		rebuildReplicas = 1
		switch {
		case cfg != nil && cfg.Deploy != nil && cfg.Deploy.Replicas > 0:
			rebuildReplicas = cfg.Deploy.Replicas
		case deployed != nil && len(deployed.Replicas) > 0:
			rebuildReplicas = len(deployed.Replicas)
		}
	case project.StrategyCanary, project.StrategyProgressive:
		plan, err := rolloutPlanFromProject(cfg, strategy)
		if err != nil {
			return err
		}
		rolloutPlan = plan
	default:
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"invalid --strategy %q\nHint: expected one of: classic, blue-green, rolling, canary, progressive", strategy)
	}
	return nil
}

func init() {
	RebuildCmd.Flags().IntVarP(&rebuildPort, "port", "p", 8080, "Port to run the application on (defaults to previous port if unspecified)")
	RebuildCmd.Flags().StringArrayVarP(&rebuildArgs, "build-arg", "a", nil, "Extra build argument to pass to the underlying build tool; can be provided multiple times")
	RebuildCmd.Flags().DurationVar(&rebuildTimeout, "build-timeout", builder.DefaultBuildTimeout,
		"How long one compiler invocation may run before it is cancelled (exit 20, BUILD_TIMEOUT)")
	RebuildCmd.Flags().StringVar(&rebuildStrategy, "strategy", "", "Deployment strategy for this rebuild only: classic, blue-green, rolling, canary, or progressive (overrides phelix.yaml, never written to it)")
	RebuildCmd.Flags().BoolVar(&rebuildBlueGreen, "blue-green", false, "Rebuild with zero-downtime blue-green deployment (requires 'phelix proxy' to be running)")
	RebuildCmd.Flags().IntVar(&rebuildReplicas, "replicas", 0, "Rebuild with zero-downtime rolling deployment over N replicas (requires 'phelix proxy' to be running)")
	RebuildCmd.Flags().IntVar(&rebuildCanary, "canary", 0, "Rebuild with a canary deployment: route N percent of traffic to the new version, verify health and metrics, then promote (requires 'phelix proxy' and an existing deployment)")
	RebuildCmd.Flags().StringVar(&rebuildTag, "tag", "", "Optional tag for this build (e.g. \"hotfix-auth-bug\"); stored as metadata alongside the auto-incremented version")
	RebuildCmd.Flags().BoolVar(&rebuildAutoRollback, "auto-rollback", false,
		"On a deploy-phase failure (start, health check, traffic switch), automatically restore the previous known-good version")
	RebuildCmd.Flags().StringVar(&rebuildSourceDir, "source-dir", "",
		"Build from this source directory instead of the app's directory (used by the webhook's isolated Git source); the app identity, output binary and deployment state stay with the app")
	RebuildCmd.Flags().BoolVar(&rebuildJSON, "json", false,
		"Output machine-readable JSON (stdout carries only the result envelope; progress moves to stderr)")
	RebuildCmd.Flags().StringVar(&rebuildRequestKey, "request-key", "",
		"Idempotency key: retrying with the same key and same inputs replays the recorded result instead of executing again; reusing a key with different inputs fails with IDEMPOTENCY_CONFLICT")
}

// runZeroDowntimeDeploy wires the deploy package into the CLI. It builds a
// deploy.Builder closure around the existing rebuildApp path, a proxy.Client
// for the control socket, and a colorised logger, then runs BlueGreen or
// Rolling depending on which flag was set. buildSource is the directory to
// compile (the app's directory, or an isolated --source-dir); the output
// binary and deployment state always stay with the app. op carries the
// operation identity: the deployment telemetry ID is correlated into the
// record and the terminal envelope is written per strategy.
func runZeroDowntimeDeploy(appInfo *app.AppInfo, name string, publicPort int, buildSource string, op *opRun) error {
	socket, err := proxy.DefaultSocketPath()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeProxy, "could not determine proxy socket path", err)
	}

	// Ensure the proxy daemon is up. If the user hasn't started it yet, spawn
	// it in the background so blue-green/rolling "just work".
	fmt.Printf("  %s Ensuring proxy daemon is running...\n", color.BlueString("→"))
	if err := proxy.EnsureDaemon(context.Background(), "", 5*time.Second); err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeProxy,
			err,
			"could not start proxy daemon\n  Start it manually with: %s",
			color.CyanString("phelix proxy"),
		)
	}

	proxyClient := proxy.NewClient(socket)
	if err := proxyClient.Ping(context.Background()); err != nil {
		return phelixerr.Wrapf(
			phelixerr.CodeConnection,
			err,
			"proxy daemon did not respond\n  Start it first with: %s",
			color.CyanString("phelix proxy"),
		)
	}

	logger := &colorLogger{}
	healthProvider := deploy.DefaultHealthProvider()

	buildMgr := builder.NewBuildManager()
	buildMgr.SetTimeout(rebuildTimeout)
	gitCommit := deploy.DetectGitCommit(buildSource)
	var lastReport *buildreport.Report
	freshSource := &deploy.FreshBuildSource{
		AppName:   name,
		AppID:     appInfo.ID,
		ExtraArgs: rebuildArgs,
		GitCommit: gitCommit,
		Tag:       rebuildTag,
		Retention: deploy.DefaultRetention{Max: 5},
		Logger:    logger,
		ReportFn:  func() *buildreport.Report { return lastReport },
		BuildFn: func(ctx context.Context, appID string, extraArgs []string) (string, error) {
			report, berr := rebuildApp(appID, extraArgs, buildMgr, buildSource)
			lastReport = report
			if berr != nil {
				return "", berr
			}
			if am, ok := app.Manager.(*app.AppManager); ok {
				if info, exists := am.Apps[appID]; exists {
					return filepath.Join(info.Directory, fmt.Sprintf("app_%s", appID)), nil
				}
			}
			return "", phelixerr.Newf(phelixerr.CodeNotFound, "could not locate built binary for %s", appID)
		},
	}

	runtimeCfg, _ := loadProjectConfigFrom(buildSource)
	deployRuntime := runtimeCfg.DeployRuntime()
	deployNetwork := runtimeCfg.DeployNetwork()
	var deploySource deploy.BuildSource = freshSource
	launcher := deploy.LauncherForApp(name)
	if deploy.IsDockerRuntime(deployRuntime) {

		deployBuild := runtimeCfg.DeployDockerBuild()
		composeFile := runtimeCfg.DeployComposeFile()
		composeService := runtimeCfg.DeployComposeService()
		deploySource = &deploy.DockerBuildSource{
			AppName:   name,
			AppID:     appInfo.ID,
			GitCommit: gitCommit,
			Tag:       rebuildTag,
			Retention: deploy.DefaultRetention{Max: 5},
			Logger:    logger,
			BuildFn:   dockerImageBuilder(buildSource, deployBuild, composeFile, composeService),
		}
		launcher = deploy.DockerLauncherForApp(name, deployNetwork)
		if deployNetwork != "" {

			pfCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			verr := deploy.VerifyDockerNetwork(pfCtx, deployNetwork)
			cancel()
			if verr != nil {
				return verr
			}
			logger.Stepf("docker runtime: attaching containers to network %q (backing services reachable by their compose DNS names)", deployNetwork)
		}
		if deployBuild == project.DockerBuildCompose {
			logger.Stepf("docker runtime: image built from compose service %q in %s (build only — runtime env stays with `phelix env`)", composeService, composeFile)
		}

		warnSvc := composeService
		if warnSvc == "" {
			warnSvc = name
		}
		ownCtx, ownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		warnIfComposeManaged(ownCtx, logger, name, warnSvc)
		ownCancel()
		logger.Stepf("docker runtime: instances run as containers (one agent, many app containers)")
	}

	release, err := deploy.AcquireDeployLock(name, "deploy")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", err)
	}

	strategy := string(deploy.ModeRolling)
	if rebuildBlueGreen {
		strategy = string(deploy.ModeBlueGreen)
	}
	if rolloutPlan != nil {
		strategy = rolloutPlan.Strategy
	}
	tracker := deploy.NewTracker(phelixgrpc.NewDeploymentSink(), appInfo.ID, name, strategy)
	op.setDeploymentID(tracker)
	defer phelixgrpc.StopDeploymentSender(5 * time.Second)

	deployCtx := context.Background()
	if rolloutPlan != nil {
		signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stopSignals()
		deployCtx = signalCtx
	}

	if rolloutPlan != nil {
		ro := &deploy.Rollout{
			AppName:        name,
			AppID:          appInfo.ID,
			PublicPort:     publicPort,
			Plan:           *rolloutPlan,
			ExtraArgs:      rebuildArgs,
			Source:         deploySource,
			Launcher:       launcher,
			Runtime:        deployRuntime,
			Network:        deployNetwork,
			ProxyClient:    proxyClient,
			HealthProvider: healthProvider,
			Logger:         logger,
			PortHandoff:    stopPublicPortOwner,
			Telemetry:      tracker,
		}
		err := ro.Deploy(deployCtx)
		reconcileAppWithDeploy(name)
		if err != nil {
			release()
			return autoRollbackAfterFailedDeploy(appInfo, name, publicPort, deploy.TargetVersionOf(deploySource), launcher,
				proxyClient, healthProvider, logger, tracker, err)
		}
		release()
		fmt.Printf("%s %s rollout complete for %s (%d steps)\n",
			color.GreenString("✓"), rolloutPlan.Strategy, color.CyanString("'%s'", name), len(rolloutPlan.Steps))
		// Build Report + regression analysis (observability only).
		emitBuildReport(name, lastReport, gitCommit,
			&deploy.RecordResult{Version: deploy.TargetVersionOf(deploySource)}, nil)
		phelixgrpc.SendVersionListForApp(appInfo.ID, name, appInfo.Directory)
		phelixgrpc.ReportBuildEventForApp(appInfo.ID, name, "classic", true,
			deploy.TargetVersionOf(deploySource), rebuildTag, gitCommit, lastReport, "")
		ver := deploy.TargetVersionOf(deploySource)
		op.setResult(&ops.Result{Version: ver, Port: publicPort, Strategy: rolloutPlan.Strategy})
		return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{
			App:      name,
			AppID:    appInfo.ID,
			Version:  ver,
			Port:     publicPort,
			Strategy: rolloutPlan.Strategy,
		}))
	}

	if rebuildBlueGreen {
		bg := &deploy.BlueGreen{
			AppName:        name,
			AppID:          appInfo.ID,
			PublicPort:     publicPort,
			ExtraArgs:      rebuildArgs,
			Source:         deploySource,
			Launcher:       launcher,
			Runtime:        deployRuntime,
			Network:        deployNetwork,
			ProxyClient:    proxyClient,
			HealthProvider: healthProvider,
			Logger:         logger,
			Telemetry:      tracker,

			PortHandoff: stopPublicPortOwner,
		}
		err := bg.Deploy(context.Background())
		reconcileAppWithDeploy(name)
		if err != nil {
			release()
			return autoRollbackAfterFailedDeploy(appInfo, name, publicPort, deploy.TargetVersionOf(deploySource), launcher,
				proxyClient, healthProvider, logger, tracker, err)
		}
		release()
		fmt.Printf("%s Zero-downtime blue-green deploy complete for %s\n", color.GreenString("✓"), color.CyanString("'%s'", name))
		// Build Report + regression analysis (observability only; the deploy
		// outcome above is already committed).
		emitBuildReport(name, lastReport, gitCommit,
			&deploy.RecordResult{Version: deploy.TargetVersionOf(deploySource)}, nil)
		phelixgrpc.SendVersionListForApp(appInfo.ID, name, appInfo.Directory)
		phelixgrpc.ReportBuildEventForApp(appInfo.ID, name, "classic", true,
			deploy.TargetVersionOf(deploySource), rebuildTag, gitCommit, lastReport, "")
		bgVer := deploy.TargetVersionOf(deploySource)
		op.setResult(&ops.Result{Version: bgVer, Port: publicPort, Strategy: "blue-green"})
		return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{
			App:      name,
			AppID:    appInfo.ID,
			Version:  bgVer,
			Port:     publicPort,
			Strategy: "blue-green",
		}))
	}

	// Rolling deploy.
	r := &deploy.Rolling{
		AppName:        name,
		AppID:          appInfo.ID,
		PublicPort:     publicPort,
		Replicas:       rebuildReplicas,
		ExtraArgs:      rebuildArgs,
		Source:         deploySource,
		Launcher:       launcher,
		Runtime:        deployRuntime,
		ProxyClient:    proxyClient,
		HealthProvider: healthProvider,
		Logger:         logger,
		PortHandoff:    stopPublicPortOwner,
		Telemetry:      tracker,
	}
	err = r.Deploy(context.Background())
	reconcileAppWithDeploy(name)
	if err != nil {
		release()
		return autoRollbackAfterFailedDeploy(appInfo, name, publicPort, deploy.TargetVersionOf(deploySource), launcher,
			proxyClient, healthProvider, logger, tracker, err)
	}
	release()
	fmt.Printf("%s Zero-downtime rolling deploy complete for %s (%d replicas)\n",
		color.GreenString("✓"), color.CyanString("'%s'", name), rebuildReplicas)
	// Build Report + regression analysis (observability only).
	emitBuildReport(name, lastReport, gitCommit,
		&deploy.RecordResult{Version: deploy.TargetVersionOf(deploySource)}, nil)
	phelixgrpc.SendVersionListForApp(appInfo.ID, name, appInfo.Directory)
	phelixgrpc.ReportBuildEventForApp(appInfo.ID, name, "classic", true,
		deploy.TargetVersionOf(deploySource), rebuildTag, gitCommit, lastReport, "")
	rollVer := deploy.TargetVersionOf(deploySource)
	op.setResult(&ops.Result{Version: rollVer, Port: publicPort, Strategy: "rolling"})
	return op.writeResultEnv(machine.Success(op.operationID(), rebuildResult{
		App:      name,
		AppID:    appInfo.ID,
		Version:  rollVer,
		Port:     publicPort,
		Strategy: "rolling",
	}))
}

// autoRollbackAfterFailedDeploy responds to one failed zero-downtime deploy
// when --auto-rollback is enabled. It is the SINGLE trigger point per failed
// deployment (called once, at the failure return), so the recovery cannot
// double-fire. Build/compile failures and user cancellations never reach it
// as triggers: RecoverableDeployFailure classifies the error, and the failed
// version must have been recorded (a pure build failure has no version to
// roll back from).
func autoRollbackAfterFailedDeploy(appInfo *app.AppInfo, name string, publicPort int,
	failedVer int, launcher deploy.InstanceLauncher, proxyClient *proxy.Client,
	healthProvider deploy.HealthConfigProvider, logger *colorLogger,
	tracker *deploy.Tracker, deployErr error) error {

	if !rebuildAutoRollback || !deploy.RecoverableDeployFailure(deployErr) {
		return deployErr
	}
	fmt.Printf("%s %v\n", color.RedString("✗"), deployErr)
	fmt.Printf("%s Automatic rollback enabled\n", color.BlueString("→"))

	if failedVer <= 0 {
		fmt.Printf("%s No version was recorded for this deployment; nothing to roll back\n",
			color.YellowString("⚠"))
		return deployErr
	}

	recoveryTracker := tracker
	if st, serr := deploy.Load(name); serr == nil && st != nil && st.Mode != "" {
		recoveryTracker = deploy.NewTracker(phelixgrpc.NewDeploymentSink(), appInfo.ID, name, string(st.Mode))
	}
	res := deploy.RunAutoRollback(context.Background(), deploy.AutoRollbackOptions{
		AppName:        name,
		AppID:          appInfo.ID,
		PublicPort:     publicPort,
		Launcher:       launcher,
		ProxyClient:    proxyClient,
		HealthProvider: healthProvider,
		Logger:         logger,
		Replicas:       rebuildReplicas,
		FailedVersion:  failedVer,
		FailureReason:  deploy.AutoRollbackReason(failedVer, deployErr),
		Telemetry:      recoveryTracker,
	})
	reconcileAppWithDeploy(name)
	if !res.Restored {
		if phelixerr.IsCode(res.Err, phelixerr.CodeRollbackTargetNotFound) {
			emitAutoRollbackEvent(appInfo, name, "", failedVer, 0,
				deploy.AutoRollbackReason(failedVer, deployErr), false, res.Err, tracker.DeploymentID())
			fmt.Printf("%s Automatic rollback was enabled, but no previous known-good version is available.\n",
				color.RedString("✗"))
			fmt.Printf("  %v\n", res.Err)
			return deployErr
		}
		tracker.Failed(deployErr)
		emitAutoRollbackEvent(appInfo, name, "", failedVer, res.ToVer,
			deploy.AutoRollbackReason(failedVer, deployErr), false, res.Err, tracker.DeploymentID())
		fmt.Printf("%s Automatic rollback failed\n", color.RedString("✗"))
		fmt.Printf("\nPrevious known-good version could not be restored safely.\n")
		fmt.Printf("Application state may require manual intervention.\n")
		return phelixerr.Wrapf(phelixerr.CodeAutoRollbackFailed, res.Err,
			"deployment of v%d failed and automatic rollback did not succeed", failedVer)
	}
	emitAutoRollbackEvent(appInfo, name, "", failedVer, res.ToVer,
		deploy.AutoRollbackReason(failedVer, deployErr), res.AlreadyServing, nil, tracker.DeploymentID())
	if res.AlreadyServing {

		fmt.Printf("%s Deployment rolled back automatically — %s kept serving\n",
			color.GreenString("✓"), color.CyanString("v%d", res.ToVer))
		return phelixerr.Wrapf(phelixerr.CodeDeployFailed, deployErr,
			"deployment of v%d failed; previous version v%d is serving again", failedVer, res.ToVer)
	}
	fmt.Printf("%s Deployment rolled back automatically\n", color.GreenString("✓"))
	return phelixerr.Wrapf(phelixerr.CodeDeployFailed, deployErr,
		"deployment of v%d failed; previous version v%d restored automatically", failedVer, res.ToVer)
}

// runClassicAutoRollback restores the previous known-good version after a
// failed CLASSIC rebuild start. The old process was already stopped before
// the failed start, so service is down; recovery restarts the known-good
// binary (brief downtime is inherent to classic). History records the
// recovery as automatic. Promotion of the failed version never happened, so
// versions.json still names the known-good version as current.
func runClassicAutoRollback(name string, appInfo *app.AppInfo, failedVer, port int, deployErr error) {
	logger := &colorLogger{}
	fmt.Printf("%s Automatic rollback enabled\n", color.BlueString("→"))
	target, err := deploy.LastKnownGoodVersion(name)
	if err != nil || target == failedVer {
		emitAutoRollbackEvent(appInfo, name, "classic", failedVer, 0,
			deploy.AutoRollbackReason(failedVer, deployErr), false,
			phelixerr.New(phelixerr.CodeRollbackTargetNotFound, "no previous known-good version available"), "")
		fmt.Printf("%s Automatic rollback was enabled, but no previous known-good version is available.\n", color.RedString("✗"))
		return
	}
	logger.Stepf("automatic rollback: restoring v%d", target)
	binPath, _, err := deploy.VersionPaths(name, target)
	if err != nil {
		emitAutoRollbackEvent(appInfo, name, "classic", failedVer, target,
			deploy.AutoRollbackReason(failedVer, deployErr), false, err, "")
		fmt.Printf("%s Automatic rollback failed: %v\n", color.RedString("✗"), err)
		return
	}
	destBin := filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", appInfo.ID))
	if err := copyFileForRollback(binPath, destBin); err != nil {
		emitAutoRollbackEvent(appInfo, name, "classic", failedVer, target,
			deploy.AutoRollbackReason(failedVer, deployErr), false, err, "")
		fmt.Printf("%s Automatic rollback failed: %v\n", color.RedString("✗"), err)
		return
	}
	if err := app.Manager.StartApplication(appInfo.ID, port, name); err != nil {
		fmt.Printf("%s Automatic rollback failed: could not start v%d: %v\n", color.RedString("✗"), target, err)
		fmt.Printf("Application state may require manual intervention.\n")
		startErr := phelixerr.Wrapf(phelixerr.CodeRollbackFailed, err, "could not start v%d", target)
		deploy.RecordRollbackResultSource(name, failedVer, target, "classic",
			deploy.AutoRollbackReason(failedVer, deployErr), nil, err, deploy.RollbackSourceAutomatic)
		emitAutoRollbackEvent(appInfo, name, "classic", failedVer, target,
			deploy.AutoRollbackReason(failedVer, deployErr), false, startErr, "")
		return
	}

	if err := deploy.PromoteVersion(name, target, "classic"); err != nil {
		logger.Warnf("could not re-promote v%d: %v", target, err)
	}
	deploy.RecordRollbackResultSource(name, failedVer, target, "classic",
		deploy.AutoRollbackReason(failedVer, deployErr), nil, nil, deploy.RollbackSourceAutomatic)
	emitAutoRollbackEvent(appInfo, name, "classic", failedVer, target,
		deploy.AutoRollbackReason(failedVer, deployErr), false, nil, "")
	logger.Successf("v%d started; previous version restored automatically", target)
}

// colorLogger implements deploy.Logger using the project's existing color
// convention (→ blue, ✓ green, ⚠ yellow, ✗ red).
type colorLogger struct{}

func (colorLogger) Stepf(f string, a ...any) {
	fmt.Printf("  %s %s\n", color.BlueString("→"), fmt.Sprintf(f, a...))
}
func (colorLogger) Infof(f string, a ...any) { fmt.Printf("    %s\n", fmt.Sprintf(f, a...)) }
func (colorLogger) Warnf(f string, a ...any) {
	fmt.Printf("  %s %s\n", color.YellowString("⚠"), fmt.Sprintf(f, a...))
}
func (colorLogger) Successf(f string, a ...any) {
	fmt.Printf("  %s %s\n", color.GreenString("✓"), fmt.Sprintf(f, a...))
}
func (colorLogger) Errorf(f string, a ...any) {
	fmt.Printf("  %s %s\n", color.RedString("✗"), fmt.Sprintf(f, a...))
}

func stopExistingApp(appInfo *app.AppInfo) error {
	if appInfo.Status != "running" {
		fmt.Printf("  %s Note: Application %s (ID: %s) was not running\n", color.YellowString("⚠"), color.CyanString("'%s'", appInfo.Name), color.YellowString(appInfo.ID))
		return nil
	}

	fmt.Printf("  %s Stopping existing application...\n", color.BlueString("→"))
	if err := app.Manager.StopApplication(appInfo.ID); err != nil {

		fmt.Printf("  %s Could not stop old process: %v\n", color.YellowString("⚠"), err)
	}
	return nil
}

// rebuildApp compiles the app and returns its captured build report so all
// rebuild paths (classic and zero-downtime) persist identical telemetry.
// sourceDir is the directory to compile — the app's directory by default, or
// an isolated source (the webhook's exact-commit worktree) passed via
// --source-dir. The output binary always lands in the app's directory.
func rebuildApp(id string, extraArgs []string, buildMgr *builder.BuildManager, sourceDir string) (*buildreport.Report, error) {
	appInfo, err := GetAppInfo(id)
	if err != nil {
		return nil, err
	}

	if appInfo.Directory == "" {
		return nil, phelixerr.Newf(phelixerr.CodeNotFound, "application directory not found for ID %s", id)
	}

	projectRoot := appInfo.Directory
	if sourceDir != "" {
		projectRoot = sourceDir
	}

	// Detect language
	lang := builder.ParseLanguage(appInfo.Language)
	if !lang.IsSupported() {
		lang = buildMgr.DetectLanguage(projectRoot)
	}

	if !lang.IsSupported() {
		return nil, phelixerr.Newf(
			phelixerr.CodeUnsupportedProject,
			"unsupported or unknown project language: %s",
			lang,
		)
	}

	outputPath := filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", id))

	fmt.Printf("  %s Preparing build environment...\n", color.BlueString("→"))

	// Prepare build context
	buildCtx, err := buildMgr.PrepareBuild(
		id,
		appInfo.Name,
		projectRoot,
		outputPath,
		lang,
		extraArgs,
	)
	if err != nil {
		printFailedBuildSummary(lang, nil)
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if info, exists := appManager.Apps[id]; exists {
				info.BuildStatus = "failed"
				info.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}
		return nil, phelixerr.Wrap(phelixerr.CodeBuildFailed, "rebuild preparation failed", err)
	}

	fmt.Printf("  %s Rebuilding with %s...\n", color.BlueString("→"), color.GreenString(buildMgr.FormatLanguage(lang)))

	if err := buildMgr.ExecuteBuild(buildCtx); err != nil {

		printFailedBuildSummary(lang, buildCtx.Config)
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if info, exists := appManager.Apps[id]; exists {
				info.BuildStatus = "failed"
				info.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}

		code := phelixerr.CodeOf(err)
		if code == phelixerr.CodeUnknown {
			code = phelixerr.CodeBuildFailed
		}

		return nil, phelixerr.Wrap(code, "rebuild failed", err)
	}

	duration := buildMgr.GetBuildDuration(buildCtx)
	fmt.Printf("  %s Rebuild completed in %s\n", color.GreenString("✓"), color.YellowString(duration.String()))

	report := newNativeBuildReport(lang, buildCtx.Config, outputPath)

	// Update build status
	if appManager, ok := app.Manager.(*app.AppManager); ok {
		if app, exists := appManager.Apps[id]; exists {
			app.BuildStatus = "success"
			app.Language = string(lang)
			app.UpdatedAt = time.Now()
			_ = appManager.SaveState()
		}
	}
	return report, nil
}
