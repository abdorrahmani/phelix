package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
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
var rebuildTag string

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

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		name, portToUse := DetermineAppParameters(appInfo, cmd, rebuildPort)

		// Precedence: CLI flag > persisted app port > phelix.yaml. The yaml is
		// consulted only when neither the flag nor the app's recorded port
		// applies, so existing managed apps keep their behavior.
		if !cmd.Flags().Changed("port") && (appInfo.Port == 0) {
			dir := currentDirOrError()
			if dir != "" && project.Exists(dir) {
				if cfg, err := project.Load(dir); err == nil && cfg.Port != 0 {
					portToUse = cfg.Port
				}
			}
		}

		if err := phelixport.Validate(portToUse); err != nil {
			return err
		}

		// Detect and validate language
		buildMgr := builder.NewBuildManager()
		lang := builder.ParseLanguage(appInfo.Language)
		if !lang.IsSupported() {
			lang = buildMgr.DetectLanguage(appInfo.Directory)
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

		// Zero-downtime deploy paths. When --blue-green or --replicas is set we
		// hand off to the deploy package instead of the stop->build->start flow
		// below. The deploy package builds via the same builder, starts the new
		// instance on an internal port, runs the tiered health check, then
		// atomically switches the proxy target — so the public port never drops
		// a connection.
		if rebuildBlueGreen || rebuildReplicas > 0 {
			return runZeroDowntimeDeploy(appInfo, name, portToUse)
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

		if err := stopExistingApp(appInfo); err != nil {
			return err
		}

		if err := rebuildApp(appInfo.ID, rebuildArgs, buildMgr); err != nil {
			return err
		}

		// --- Version recording ------------------------------------------------
		// Same two-phase invariant as build: record with is_current=false,
		// promote only after the start succeeds.
		gitCommit := deploy.DetectGitCommit(appInfo.Directory)
		logger := &colorLogger{}
		rec, verErr := deploy.RecordFreshBuild(
			name, appInfo.ID,
			filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", appInfo.ID)),
			gitCommit, rebuildTag,
			deploy.DefaultRetention{Max: 5}, logger,
		)
		if verErr != nil {
			fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
		} else {
			fmt.Printf("  %s Recorded version v%d\n", color.BlueString("→"), rec.Version)
		}

		fmt.Printf("  %s Starting application on port %d...\n", color.BlueString("→"), portToUse)
		if err := app.Manager.StartApplication(appInfo.ID, portToUse, name); err != nil {
			// Deploy failed. Version exists on disk but is_current is
			// false and PromoteVersion was never called.
			return phelixerr.Wrapf(
				phelixerr.CodeProcessFailed,
				err,
				"failed to start rebuilt application %q (ID: %s)",
				name,
				appInfo.ID,
			)
		}

		// Deploy succeeded — promote the version.
		if rec != nil {
			if err := deploy.PromoteVersion(name, rec.Version, "classic"); err != nil {
				fmt.Printf("  %s Warning: could not promote version: %v\n", color.YellowString("⚠"), err)
			}
		}

		fmt.Printf("%s Application %s (ID: %s) rebuilt and started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), portToUse)
		phelixgrpc.ReportEvent(appInfo.ID, name, "rebuild", true, "", 0, "", "")
		phelixgrpc.SendVersionListForApp(appInfo.ID, name, appInfo.Directory)
		return nil
	},
}

func init() {
	RebuildCmd.Flags().IntVarP(&rebuildPort, "port", "p", 8080, "Port to run the application on (defaults to previous port if unspecified)")
	RebuildCmd.Flags().StringArrayVarP(&rebuildArgs, "build-arg", "a", nil, "Extra build argument to pass to the underlying build tool; can be provided multiple times")
	RebuildCmd.Flags().BoolVar(&rebuildNoUpload, "no-upload", false, "If set, do not upload/send app information to the server after rebuild")
	RebuildCmd.Flags().BoolVar(&rebuildBlueGreen, "blue-green", false, "Rebuild with zero-downtime blue-green deployment (requires 'phelix proxy' to be running)")
	RebuildCmd.Flags().IntVar(&rebuildReplicas, "replicas", 0, "Rebuild with zero-downtime rolling deployment over N replicas (requires 'phelix proxy' to be running)")
	RebuildCmd.Flags().StringVar(&rebuildTag, "tag", "", "Optional tag for this build (e.g. \"hotfix-auth-bug\"); stored as metadata alongside the auto-incremented version")
}

// runZeroDowntimeDeploy wires the deploy package into the CLI. It builds a
// deploy.Builder closure around the existing rebuildApp path, a proxy.Client
// for the control socket, and a colorised logger, then runs BlueGreen or
// Rolling depending on which flag was set.
func runZeroDowntimeDeploy(appInfo *app.AppInfo, name string, publicPort int) error {
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
	// handles language detection, toolchain checks and env injection.
	buildMgr := builder.NewBuildManager()
	gitCommit := deploy.DetectGitCommit(appInfo.Directory)
	freshSource := &deploy.FreshBuildSource{
		AppName:   name,
		AppID:     appInfo.ID,
		ExtraArgs: rebuildArgs,
		GitCommit: gitCommit,
		Tag:       rebuildTag,
		Retention: deploy.DefaultRetention{Max: 5},
		Logger:    logger,
		BuildFn: func(ctx context.Context, appID string, extraArgs []string) (string, error) {
			if err := rebuildApp(appID, extraArgs, buildMgr); err != nil {
				return "", err
			}
			if am, ok := app.Manager.(*app.AppManager); ok {
				if info, exists := am.Apps[appID]; exists {
					return filepath.Join(info.Directory, fmt.Sprintf("app_%s", appID)), nil
				}
			}
			return "", phelixerr.Newf(phelixerr.CodeNotFound, "could not locate built binary for %s", appID)
		},
	}

	release, err := deploy.AcquireDeployLock(name, "deploy")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeDeployLocked, "could not acquire deploy lock", err)
	}
	defer release()

	if rebuildBlueGreen {
		bg := &deploy.BlueGreen{
			AppName:        name,
			AppID:          appInfo.ID,
			PublicPort:     publicPort,
			ExtraArgs:      rebuildArgs,
			Source:         freshSource,
			Launcher:       deploy.DefaultLauncher,
			ProxyClient:    proxyClient,
			HealthProvider: healthProvider,
			Logger:         logger,
		}
		if err := bg.Deploy(context.Background()); err != nil {
			return err
		}
		fmt.Printf("%s Zero-downtime blue-green deploy complete for %s\n", color.GreenString("✓"), color.CyanString("'%s'", name))
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
		Source:         freshSource,
		Launcher:       deploy.DefaultLauncher,
		ProxyClient:    proxyClient,
		HealthProvider: healthProvider,
		Logger:         logger,
	}
	if err := r.Deploy(context.Background()); err != nil {
		return err
	}
	fmt.Printf("%s Zero-downtime rolling deploy complete for %s (%d replicas)\n",
		color.GreenString("✓"), color.CyanString("'%s'", name), rebuildReplicas)
	phelixgrpc.SendVersionListForApp(appInfo.ID, name, appInfo.Directory)
	return nil
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

func rebuildApp(id string, extraArgs []string, buildMgr *builder.BuildManager) error {
	appInfo, err := GetAppInfo(id)
	if err != nil {
		return err
	}

	if appInfo.Directory == "" {
		return phelixerr.Newf(phelixerr.CodeNotFound, "application directory not found for ID %s", id)
	}

	projectRoot := appInfo.Directory

	// Detect language
	lang := builder.ParseLanguage(appInfo.Language)
	if !lang.IsSupported() {
		lang = buildMgr.DetectLanguage(projectRoot)
	}

	if !lang.IsSupported() {
		return phelixerr.Newf(
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
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if info, exists := appManager.Apps[id]; exists {
				info.BuildStatus = "failed"
				info.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}
		return phelixerr.Wrap(phelixerr.CodeBuildFailed, "rebuild preparation failed", err)
	}

	fmt.Printf("  %s Rebuilding with %s...\n", color.BlueString("→"), color.GreenString(buildMgr.FormatLanguage(lang)))

	// Execute build
	if err := buildMgr.ExecuteBuild(buildCtx); err != nil {
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if info, exists := appManager.Apps[id]; exists {
				info.BuildStatus = "failed"
				info.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}
		return phelixerr.Wrap(phelixerr.CodeBuildFailed, "rebuild failed", err)
	}

	duration := buildMgr.GetBuildDuration(buildCtx)
	fmt.Printf("  %s Rebuild completed in %s\n", color.GreenString("✓"), color.YellowString(duration.String()))

	// Update build status
	if appManager, ok := app.Manager.(*app.AppManager); ok {
		if app, exists := appManager.Apps[id]; exists {
			app.BuildStatus = "success"
			app.Language = string(lang)
			app.UpdatedAt = time.Now()
			_ = appManager.SaveState()
		}
	}
	return nil
}
