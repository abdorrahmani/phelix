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
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/abdorrahmani/phelix/internal/toolchain"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var rebuildPort int
var rebuildArgs []string
var rebuildNoUpload bool
var rebuildBlueGreen bool
var rebuildReplicas int
var rebuildStrategy string
var rebuildTag string
var rebuildAutoRollback bool
var rebuildCanary int
var rebuildSourceDir string

// rolloutPlan is non-nil when this rebuild takes the canary/progressive path.
// It is resolved by applyConfigDeployStrategy from --canary, --strategy or
// phelix.yaml before the zero-downtime branch dispatches on it.
var rolloutPlan *deploy.RolloutPlan

var RebuildCmd = &cobra.Command{
	Use:   "rebuild [ID|AppName] --port <PORT>",
	Short: "Rebuilds and runs a Go Application by its ID or AppName.",
	Args:  cobra.MaximumNArgs(1),
	// Deploy failures are already printed with context (✗ lines); cobra's
	// default usage dump and "Error:" prefix after a long blue-green/rolling
	// run are noise.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Project configuration (phelix.yaml) supplies name/port/strategy
		// defaults. Missing file is fine; an invalid file fails fast.
		projCfg, err := loadProjectConfig()
		if err != nil {
			return err
		}

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		if len(args) == 0 && projCfg != nil && projCfg.Name != "" {
			// phelix.yaml names the app; use it when that app exists so
			// rebuild inside the project directory needs no argument.
			if _, err := GetAppInfo(projCfg.Name); err == nil {
				args = []string{projCfg.Name}
			}
		}
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix rebuild <ID|AppName> --port <PORT>")
			}
			identifier, err := PromptApp(false, "Select application to rebuild")
			if err != nil {
				return err
			}
			args = []string{identifier}
		}

		identifier := args[0]

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		// --source-dir builds from an isolated source directory (the webhook's
		// exact-commit Git worktree) instead of the app's directory. The app
		// identity, output binary path, deployment state and ports stay with
		// the app; only the compiled source and the git metadata come from
		// this directory. phelix.yaml is read from it too, so a push that
		// changes the project configuration deploys with its own config.
		buildSource := appInfo.Directory
		if rebuildSourceDir != "" {
			abs, err := filepath.Abs(rebuildSourceDir)
			if err != nil {
				return phelixerr.Wrapf(phelixerr.CodeInvalidArgument, err, "--source-dir %q could not be resolved", rebuildSourceDir)
			}
			if st, serr := os.Stat(abs); serr != nil || !st.IsDir() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "--source-dir %q is not an existing directory", rebuildSourceDir)
			}
			buildSource = abs
			// The project configuration governs the deploy; with an isolated
			// source that is the configuration as pushed, not the one in the
			// daemon's working directory.
			sourceCfg, serr := loadProjectConfigFrom(buildSource)
			if serr != nil {
				return serr
			}
			if sourceCfg != nil {
				projCfg = sourceCfg
			}
		}

		// Resource policy follows this app's source, never the invoking directory.
		resourceCfg, err := loadProjectConfigFrom(buildSource)
		if err != nil {
			return err
		}
		if err := syncProjectResources(resourceCfg, appInfo.ID); err != nil {
			return err
		}

		name, portToUse := DetermineAppParameters(appInfo, cmd, rebuildPort)

		// Precedence: CLI flag > persisted app port > phelix.yaml. The yaml is
		// consulted only when neither the flag nor the app's recorded port
		// applies, so existing managed apps keep their behavior.
		if !cmd.Flags().Changed("port") && (appInfo.Port == 0) && projCfg != nil && projCfg.Port != 0 {
			portToUse = projCfg.Port
		}

		// Deployment strategy: --strategy (one-off, e.g. a backend-issued
		// rebuild) beats phelix.yaml, explicit --blue-green/--replicas beat
		// both, and an app already running blue-green/rolling keeps that
		// strategy when nothing names one. Classic stays the default for apps
		// with no deployment.
		if err := applyConfigDeployStrategy(cmd, projCfg, name); err != nil {
			return err
		}

		if err := phelixport.Validate(portToUse); err != nil {
			return err
		}

		// Detect and validate language (from the source being built)
		buildMgr := builder.NewBuildManager()
		lang := builder.ParseLanguage(appInfo.Language)
		if !lang.IsSupported() {
			lang = buildMgr.DetectLanguage(buildSource)
		}

		if !lang.IsSupported() {
			return phelixerr.Newf(
				phelixerr.CodeUnsupportedProject,
				"unsupported or unknown project language: %s",
				lang,
			)
		}

		// Check toolchain; prompt to install if missing
		fmt.Printf("  %s Checking toolchain...\n", color.BlueString("→"))
		if err := toolchain.EnsureTool(lang, Confirm); err != nil {
			return phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain check failed", err)
		}

		fmt.Printf("%s Rebuilding application %s (ID: %s)\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID))
		fmt.Printf("  Language: %s\n", color.GreenString(buildMgr.FormatLanguage(lang)))

		// If user set no-upload flag, update the app info
		if rebuildNoUpload {
			appInfo.NoUpload = true
			_ = app.Manager.SaveState()
		}

		// Apply the watching value declared in phelix.yaml (desired state) so
		// a project that declares enable/disable converges on every rebuild.
		// A file without the key leaves the persisted flag untouched.
		if serr := syncProjectWatching(projCfg, appInfo.ID); serr != nil {
			fmt.Printf("  %s Warning: could not apply watching from %s: %v\n",
				color.YellowString("⚠"), project.FileName, serr)
		}

		// Apply health endpoints declared in phelix.yaml (desired state) so
		// both the classic start and the zero-downtime deploy tiers see them.
		if serr := syncProjectHealth(projCfg, appInfo.ID, name, portToUse); serr != nil {
			fmt.Printf("  %s Warning: could not apply health endpoints from %s: %v\n",
				color.YellowString("⚠"), project.FileName, serr)
		}

		// Zero-downtime deploy paths. When --blue-green, --replicas or a
		// canary/progressive rollout is selected we hand off to the deploy
		// package instead of the stop->build->start flow below. The deploy
		// package builds via the same builder, starts the new instance on an
		// internal port, runs the tiered health check, then atomically
		// switches the proxy target — so the public port never drops a
		// connection.
		if rebuildBlueGreen || rebuildReplicas > 0 || rolloutPlan != nil {
			return runZeroDowntimeDeploy(appInfo, name, portToUse, buildSource)
		}

		// A classic rebuild of an app still managed by blue-green/rolling is
		// the documented migration to classic: tear the deployment down first
		// so no replica survives as an orphan and no proxy route fights the
		// new classic process for the public port.
		if err := migrateToClassic(name); err != nil {
			return phelixerr.Wrapf(phelixerr.CodeDeployFailed, err, "could not migrate %q to classic deployment", name)
		}

		// Acquire deploy lock FIRST so two concurrent rebuilds cannot race on
		// versions.json or double-assign version numbers; previously the old
		// process was stopped before locking, letting a concurrent rebuild
		// interleave between the stop and the lock.
		release, lockErr := deploy.AcquireDeployLock(name, "rebuild")
		if lockErr != nil {
			return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", lockErr)
		}
		defer release()

		currentVer, _ := deploy.CurrentVersion(name)
		tracker, flushTelemetry := classicTracker(appInfo.ID, name, portToUse, currentVer)
		defer flushTelemetry()

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

		// --- Version recording ------------------------------------------------
		// Same two-phase invariant as build: record with is_current=false,
		// promote only after the start succeeds. The git commit is detected
		// from the source that was actually compiled, so version metadata
		// never claims a commit the binary was not built from.
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

		// --- Build Report + regression analysis ---------------------------------
		emitBuildReport(name, rebuildReport, gitCommit, rec, verErr)

		fmt.Printf("  %s Starting application on port %d...\n", color.BlueString("→"), portToUse)
		if err := app.Manager.StartApplication(appInfo.ID, portToUse, name); err != nil {
			// Deploy failed. Version exists on disk but is_current is
			// false and PromoteVersion was never called.
			startErr := phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to start rebuilt application %q (ID: %s)",
				name,
				appInfo.ID,
			)
			tracker.Failed(startErr)
			if rec != nil && rebuildAutoRollback {
				// The version was recorded but never promoted; the previous
				// classic process was stopped above, so the old version must
				// be restarted to restore service. Brief downtime is
				// unavoidable here — classic has no second slot.
				runClassicAutoRollback(name, appInfo, rec.Version, portToUse, startErr)
			}
			return startErr
		}
		tracker.InstanceStarted("", classicPID(appInfo.ID), portToUse)

		// Deploy succeeded — promote the version.
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
		return nil
	},
}

// applyConfigDeployStrategy resolves which deployment path this rebuild takes
// and maps it onto the existing --blue-green / --replicas flags or a rollout
// plan.
//
// Precedence: explicit --blue-green/--replicas/--canary > --strategy >
// phelix.yaml > the strategy the app is currently deployed with > classic.
// --strategy is the one-off override (the backend's remote rebuild uses it);
// it is never written back to phelix.yaml.
//
// Inheriting the deployed strategy matters: falling straight through to classic
// meant any rebuild that named no strategy — a phelix.yaml without a deploy
// block, or a backend-issued rebuild that sent no override — silently migrated
// a live blue-green/rolling app to classic, tearing down its instances and
// deleting deploy.json along with the mode, public port, active version, health
// tier and rollback record it holds. Demoting a deployment destroys state, so
// it has to be asked for (--strategy classic, or deploy.strategy in
// phelix.yaml), not defaulted into.
//
// A canary/progressive rollout runs on the blue-green topology, so an app whose
// last deploy was a rollout inherits blue-green when nothing names a strategy —
// a safe full cut-over, never a destructive downgrade. Put deploy.strategy:
// canary/progressive in phelix.yaml to make rollouts the app's default.
func applyConfigDeployStrategy(cmd *cobra.Command, cfg *project.Config, appName string) error {
	// The explicit canary flag: one-shot rollout at the given traffic share.
	// It names a strategy just like --blue-green does, so combining them is a
	// contradiction rather than a precedence question.
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
	deployed := loadDeployState(appName)
	if strategy == "" && deployed != nil {
		strategy = string(deployed.Mode)
	}

	switch strategy {
	case "", project.StrategyClassic:
		// Classic is the default path; nothing to set.
	case project.StrategyBlueGreen:
		rebuildBlueGreen = true
	case project.StrategyRolling:
		// Rolling needs a replica count. phelix.yaml supplies one when it has
		// it — including for an override that only named the strategy — then
		// the width the app is already running at (so an inherited rolling
		// rebuild does not silently shrink it), and 1 is the floor.
		rebuildReplicas = 1
		switch {
		case cfg != nil && cfg.Deploy != nil && cfg.Deploy.Replicas > 0:
			rebuildReplicas = cfg.Deploy.Replicas
		case deployed != nil && len(deployed.Replicas) > 0:
			rebuildReplicas = len(deployed.Replicas)
		}
	case project.StrategyCanary, project.StrategyProgressive:
		// The rollout plan (steps, verification window, thresholds) lives in
		// phelix.yaml; project.Load has already validated its shape.
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
	RebuildCmd.Flags().BoolVar(&rebuildNoUpload, "no-upload", false, "If set, do not upload/send app information to the server after rebuild")
	RebuildCmd.Flags().StringVar(&rebuildStrategy, "strategy", "", "Deployment strategy for this rebuild only: classic, blue-green, rolling, canary, or progressive (overrides phelix.yaml, never written to it)")
	RebuildCmd.Flags().BoolVar(&rebuildBlueGreen, "blue-green", false, "Rebuild with zero-downtime blue-green deployment (requires 'phelix proxy' to be running)")
	RebuildCmd.Flags().IntVar(&rebuildReplicas, "replicas", 0, "Rebuild with zero-downtime rolling deployment over N replicas (requires 'phelix proxy' to be running)")
	RebuildCmd.Flags().IntVar(&rebuildCanary, "canary", 0, "Rebuild with a canary deployment: route N percent of traffic to the new version, verify health and metrics, then promote (requires 'phelix proxy' and an existing deployment)")
	RebuildCmd.Flags().StringVar(&rebuildTag, "tag", "", "Optional tag for this build (e.g. \"hotfix-auth-bug\"); stored as metadata alongside the auto-incremented version")
	RebuildCmd.Flags().BoolVar(&rebuildAutoRollback, "auto-rollback", false,
		"On a deploy-phase failure (start, health check, traffic switch), automatically restore the previous known-good version")
	RebuildCmd.Flags().StringVar(&rebuildSourceDir, "source-dir", "",
		"Build from this source directory instead of the app's directory (used by the webhook's isolated Git source); the app identity, output binary and deployment state stay with the app")
}

// runZeroDowntimeDeploy wires the deploy package into the CLI. It builds a
// deploy.Builder closure around the existing rebuildApp path, a proxy.Client
// for the control socket, and a colorised logger, then runs BlueGreen or
// Rolling depending on which flag was set. buildSource is the directory to
// compile (the app's directory, or an isolated --source-dir); the output
// binary and deployment state always stay with the app.
func runZeroDowntimeDeploy(appInfo *app.AppInfo, name string, publicPort int, buildSource string) error {
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

	// deploy.Builder closure: delegates to the existing rebuild path, which
	// handles language detection, toolchain checks and env injection. The
	// captured build report is handed to FreshBuildSource via ReportFn so the
	// recorded version carries the same telemetry as classic builds. The git
	// commit is detected from the source actually compiled.
	buildMgr := builder.NewBuildManager()
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

	// Resolve the launch runtime from the app's own source config (PHELIX_RUNTIME
	// env → phelix.yaml deploy.runtime → native). Native keeps the existing
	// binary source+launcher untouched; docker swaps in the image source and the
	// container launcher — the deploy engines and proxy path are identical.
	runtimeCfg, _ := loadProjectConfigFrom(buildSource)
	deployRuntime := runtimeCfg.DeployRuntime()
	deployNetwork := runtimeCfg.DeployNetwork()
	var deploySource deploy.BuildSource = freshSource
	launcher := deploy.LauncherForApp(name)
	if deploy.IsDockerRuntime(deployRuntime) {
		// How the image is built (dockerfile default | compose) — this only
		// changes where the image comes from; the launcher/proxy/network below
		// are identical either way.
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
			// Fail fast before any image build or container run: a missing user
			// network means containers could never resolve the backing services
			// on it, so abort now with an actionable message rather than after a
			// full image build. Bounded so a wedged daemon cannot hang the deploy.
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
		// Warn (never block) if the same app also runs as a compose-managed
		// container — the app tier would then have two owners. Silent under the
		// compose-profile pattern (no such container runs on the server).
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
	// The lock is NOT deferred: automatic rollback must acquire it again after
	// the failed deploy returns (same-process flock excludes re-acquisition),
	// and the recovery path below releases it explicitly first.

	// Deployment telemetry. The sink is nil when the CLI has no session, which
	// makes the tracker nil and the deploy identical to an offline run. Queued
	// events are flushed before the command returns.
	strategy := string(deploy.ModeRolling)
	if rebuildBlueGreen {
		strategy = string(deploy.ModeBlueGreen)
	}
	if rolloutPlan != nil {
		strategy = rolloutPlan.Strategy
	}
	tracker := deploy.NewTracker(phelixgrpc.NewDeploymentSink(), appInfo.ID, name, strategy)
	defer phelixgrpc.StopDeploymentSender(5 * time.Second)

	// Canary/progressive rollouts run step windows that can take minutes; a
	// Ctrl-C must abort the rollout through its cancellation path (which
	// restores the stable version to 100% of traffic) instead of killing the
	// CLI mid-switch. Blue-green and rolling keep their existing behavior.
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
		return nil
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
			// Classic → blue-green migration: when the app still runs as a
			// classic process binding the public port, stop it right before
			// the proxy enrols (after the candidate is healthy), so the user
			// never has to run 'phelix stop' by hand.
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
		return nil
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
	return nil
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
		// No version was recorded (build failed after PrepareBuild but the
		// error still classified as deploy-phase): nothing to roll back from.
		fmt.Printf("%s No version was recorded for this deployment; nothing to roll back\n",
			color.YellowString("⚠"))
		return deployErr
	}

	// The recovery is its own deployment operation: give it a fresh tracker
	// (its own deployment_id) so its transitions never mix with the failed
	// deploy's, matching how a manual rollback reports. Silent when the
	// deploy state is unreadable — telemetry must never block recovery.
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
			// No previous known-good version: report it, do not fabricate a
			// rollback target.
			emitAutoRollbackEvent(appInfo, name, "", failedVer, 0,
				deploy.AutoRollbackReason(failedVer, deployErr), false, res.Err)
			fmt.Printf("%s Automatic rollback was enabled, but no previous known-good version is available.\n",
				color.RedString("✗"))
			fmt.Printf("  %v\n", res.Err)
			return deployErr
		}
		tracker.Failed(deployErr)
		emitAutoRollbackEvent(appInfo, name, "", failedVer, res.ToVer,
			deploy.AutoRollbackReason(failedVer, deployErr), false, res.Err)
		fmt.Printf("%s Automatic rollback failed\n", color.RedString("✗"))
		fmt.Printf("\nPrevious known-good version could not be restored safely.\n")
		fmt.Printf("Application state may require manual intervention.\n")
		return phelixerr.Wrapf(phelixerr.CodeAutoRollbackFailed, res.Err,
			"deployment of v%d failed and automatic rollback did not succeed", failedVer)
	}
	emitAutoRollbackEvent(appInfo, name, "", failedVer, res.ToVer,
		deploy.AutoRollbackReason(failedVer, deployErr), res.AlreadyServing, nil)
	if res.AlreadyServing {
		// The failure never reached traffic (blue-green pre-switch abort,
		// rolling failure at the first replica): the known-good version kept
		// serving the whole time.
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
			phelixerr.New(phelixerr.CodeRollbackTargetNotFound, "no previous known-good version available"))
		fmt.Printf("%s Automatic rollback was enabled, but no previous known-good version is available.\n", color.RedString("✗"))
		return
	}
	logger.Stepf("automatic rollback: restoring v%d", target)
	binPath, _, err := deploy.VersionPaths(name, target)
	if err != nil {
		emitAutoRollbackEvent(appInfo, name, "classic", failedVer, target,
			deploy.AutoRollbackReason(failedVer, deployErr), false, err)
		fmt.Printf("%s Automatic rollback failed: %v\n", color.RedString("✗"), err)
		return
	}
	destBin := filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", appInfo.ID))
	if err := copyFileForRollback(binPath, destBin); err != nil {
		emitAutoRollbackEvent(appInfo, name, "classic", failedVer, target,
			deploy.AutoRollbackReason(failedVer, deployErr), false, err)
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
			deploy.AutoRollbackReason(failedVer, deployErr), false, startErr)
		return
	}
	// Promotion failed earlier only as a warning path — here the known-good
	// version is already current in versions.json; re-promote is idempotent.
	if err := deploy.PromoteVersion(name, target, "classic"); err != nil {
		logger.Warnf("could not re-promote v%d: %v", target, err)
	}
	deploy.RecordRollbackResultSource(name, failedVer, target, "classic",
		deploy.AutoRollbackReason(failedVer, deployErr), nil, nil, deploy.RollbackSourceAutomatic)
	emitAutoRollbackEvent(appInfo, name, "classic", failedVer, target,
		deploy.AutoRollbackReason(failedVer, deployErr), false, nil)
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
		// Warn but don't block the rebuild — the old process may be
		// unkillable (e.g. stuck in D-state I/O). The new build will
		// fail with a clear port-conflict error if the old process still
		// holds the port.
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
		appInfo.NoUpload,
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

	// Execute build
	if err := buildMgr.ExecuteBuild(buildCtx); err != nil {
		// Failed builds are never recorded as versions; print post-mortem.
		printFailedBuildSummary(lang, buildCtx.Config)
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if info, exists := appManager.Apps[id]; exists {
				info.BuildStatus = "failed"
				info.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}
		return nil, phelixerr.Wrap(phelixerr.CodeBuildFailed, "rebuild failed", err)
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
