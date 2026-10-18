package cmd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/cmd/auth"
	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/ops"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/toolchain"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

const (
	defaultPort = 8080
)

var (
	buildPort int
	buildArgs []string
	buildTag  string

	// Matrix build flags.
	matrixFlag        bool
	goVersions        []string
	rustVersions      []string
	platforms         []string
	matrixConcurrency int
	matrixDryRun      bool
	// matrixRetries is the automatic-retry budget for failed combinations.
	matrixRetries int
	// matrixResume selects resume mode: "" (off), "latest", or a run ID.
	matrixResume string
	buildDebug   bool
	buildJSON    bool
)

var BuildCmd = &cobra.Command{
	Use:   "build [NAME] --port <PORT>",
	Short: "Builds and runs an application (Go/Rust) with a specified name",
	Long:  "Compiles an application from the current directory (auto-detects language) with the given name and starts it immediately",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) (err error) {
		if buildJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		projCfg, err := loadProjectConfig()
		if err != nil {
			return err
		}

		if matrixResume != "" {
			return resumeMatrixRun(args, projCfg, buildArgs, buildTag, buildDebug)
		}

		if len(args) == 0 && projCfg != nil && projCfg.Name != "" {
			args = []string{projCfg.Name}
		}
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <NAME>; usage: phelix build <NAME> --port <PORT> (or set name: in phelix.yaml)")
			}
			entered, err := PromptString("Application name", "")
			if err != nil {
				return err
			}
			if err := validateName(entered); err != nil {
				return err
			}
			args = []string{entered}
		}

		name := args[0]

		if ferr := matrixFlagConflict(cmd.Flags().Changed("matrix"), matrixFlag, goVersions, rustVersions, platforms); ferr != nil {
			return ferr
		}

		isMatrix := matrixActive(cmd, projCfg)
		if isMatrix {
			if ferr := validateMatrixFlagValues(matrixConcurrency, cmd.Flags().Changed("matrix-concurrency"), matrixRetries, cmd.Flags().Changed("matrix-retries")); ferr != nil {
				return ferr
			}
		}
		if !cmd.Flags().Changed("port") && !isMatrix {
			if projCfg != nil && projCfg.Port != 0 {
				buildPort = projCfg.Port
			}

			if projCfg == nil || projCfg.Port == 0 {
				if IsInteractive() {
					chosen, err := PromptInt("Port to run the application on", buildPort)
					if err != nil {
						return err
					}
					buildPort = chosen
				}
			}
		}

		var buildListener net.Listener
		defer func() {
			if buildListener != nil {
				_ = buildListener.Close()
			}
		}()

		if !isMatrix {
			if err := phelixport.Validate(buildPort); err != nil {
				return err
			}

			ln, err := phelixport.ReservePort(buildPort)
			if err != nil {
				return err
			}
			buildListener = ln
		}

		if !auth.IsLoggedIn() {
			fmt.Printf("  %s Not logged in — this build will run normally, but no metrics or events will be sent to the phelix.anophel.com dashboard. Run %s to enable monitoring.\n",
				color.YellowString("Note:"), color.CyanString("'phelix auth login'"))
		}

		if err := validateName(name); err != nil {
			return err
		}

		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
		}

		currentDir, err := os.Getwd()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
		}

		buildMgr := builder.NewBuildManager()
		lang := buildMgr.DetectLanguage(currentDir)
		if !lang.IsSupported() {
			return phelixerr.Newf(
				phelixerr.CodeUnsupportedProject,
				"unsupported or unknown project language: %s",
				lang,
			)
		}

		var op *opRun
		if !isMatrix {
			op = newOpRun(ops.KindBuild, name, "")
			op.beginRecord()
			defer func() { op.finish(&err) }()
		}
		if isMatrix {
			prof, rerr := resolveMatrixProfile(cmd, projCfg, lang)
			if rerr != nil {
				return rerr
			}
			return runMatrixMode(name, currentDir, buildArgs, buildTag, prof)
		}

		if err := validateUniqueName(name); err != nil {
			return err
		}

		if projCfg.DeployRuntime() == project.RuntimeDocker {
			return runDockerInitialBuild(cmd, projCfg, name, buildPort, currentDir, lang, op)
		}

		fmt.Printf("  %s Checking toolchain...\n", color.BlueString("→"))
		if err := toolchain.EnsureTool(lang, Confirm); err != nil {
			return phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain check failed", err)
		}

		id := app.Manager.GenerateAppID()
		fmt.Printf("%s Building application %s (ID: %s)\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(id))
		fmt.Printf("  Language: %s\n", color.GreenString(buildMgr.FormatLanguage(lang)))

		if err := createAppEntry(id, name, lang); err != nil {
			return err
		}
		if err := syncProjectResources(projCfg, id); err != nil {
			return err
		}

		if serr := syncProjectWatching(projCfg, id); serr != nil {
			fmt.Printf("  %s Warning: could not apply watching from %s: %v\n",
				color.YellowString("⚠"), project.FileName, serr)
		}

		if serr := syncProjectHealth(projCfg, id, name, buildPort); serr != nil {
			fmt.Printf("  %s Warning: could not apply health endpoints from %s: %v\n",
				color.YellowString("⚠"), project.FileName, serr)
		}

		binPath := filepath.Join(currentDir, fmt.Sprintf("app_%s", id))
		tracker, flushTelemetry := classicTracker(id, name, buildPort, 0)
		defer flushTelemetry()
		op.setDeploymentID(tracker)

		tracker.Building("classic build")
		report, err := buildApplication(id, buildArgs, buildMgr, binPath)
		if err != nil {
			tracker.Failed(err)
			return err
		}

		gitCommit := deploy.DetectGitCommit(currentDir)
		logger := &colorLogger{}
		rec, verErr := deploy.RecordFreshBuild(
			name, id,
			binPath,
			gitCommit, buildTag,
			report,
			deploy.DefaultRetention{Max: 5}, logger,
		)
		if verErr != nil {
			fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
		} else {
			fmt.Printf("  %s Recorded version v%d\n", color.BlueString("→"), rec.Version)
			tracker.SetTargetVersion(rec.Version)
		}

		emitBuildReport(name, report, gitCommit, rec, verErr)

		buildVer := 0
		if rec != nil {
			buildVer = rec.Version
		}
		phelixgrpc.ReportBuildEventForApp(id, name, "classic", true, buildVer, buildTag, gitCommit, report, "")

		if buildListener != nil {
			_ = buildListener.Close()
			buildListener = nil
		}

		if err := startApplication(id, name); err != nil {
			tracker.Failed(err)
			return err
		}

		tracker.InstanceStarted("", classicPID(id), buildPort)

		if rec != nil {
			if err := deploy.PromoteVersion(name, rec.Version, "classic"); err != nil {
				fmt.Printf("  %s Warning: could not promote version: %v\n", color.YellowString("⚠"), err)
			}
		}
		tracker.PromoteCurrentVersion()
		tracker.Completed(fmt.Sprintf("running on port %d", buildPort))

		fmt.Printf("%s Application %s (ID: %s) started successfully on port %d\n",
			color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(id), buildPort)

		phelixgrpc.ReportEvent(id, name, "build", true, "", 0, "classic", "")
		phelixgrpc.SendVersionListForApp(id, name, currentDir)

		buildResVer := 0
		if rec != nil {
			buildResVer = rec.Version
		}
		op.setResult(&ops.Result{Version: buildResVer, Port: buildPort, Strategy: "classic"})
		return op.writeResultEnv(machine.Success(op.operationID(), buildResult{
			App:     name,
			AppID:   id,
			Version: buildResVer,
			Path:    binPath,
		}))
	},
}

func init() {
	BuildCmd.Flags().IntVarP(&buildPort, "port", "p", defaultPort, "Port to run the application on")
	BuildCmd.Flags().StringArrayVarP(&buildArgs, "build-arg", "a", nil, "Extra build argument to pass to the underlying build tool; can be provided multiple times")
	BuildCmd.Flags().StringVar(&buildTag, "tag", "", "Optional tag for this build (e.g. \"hotfix-auth-bug\"); stored as metadata alongside the auto-incremented version")

	// Matrix build flags.
	BuildCmd.Flags().BoolVar(&buildJSON, "json", false,
		"Output machine-readable JSON (stdout carries only the result envelope; progress moves to stderr)")
	BuildCmd.Flags().BoolVar(&matrixFlag, "matrix", false, "Enable matrix build mode (cross-product of versions × platforms)")
	BuildCmd.Flags().StringSliceVar(&goVersions, "go-versions", nil, "Go versions to build with (e.g. 1.21,1.22,1.23)")
	BuildCmd.Flags().StringSliceVar(&rustVersions, "rust-versions", nil, "Rust versions to build with (e.g. 1.77,1.78)")
	BuildCmd.Flags().StringSliceVar(&platforms, "platforms", nil, "Target platforms (e.g. linux/amd64,linux/arm64)")
	BuildCmd.Flags().IntVar(&matrixConcurrency, "matrix-concurrency", matrix.DefaultConcurrency, "Max parallel builds in matrix mode")
	BuildCmd.Flags().IntVar(&matrixRetries, "matrix-retries", 0, "Retry failed matrix combinations up to N additional times (transient failures only)")
	BuildCmd.Flags().StringVar(&matrixResume, "resume", "", "Resume an interrupted matrix run: 'latest' or a run ID (implies matrix mode)")
	BuildCmd.Flags().Lookup("resume").NoOptDefVal = "latest"
	BuildCmd.Flags().BoolVar(&matrixDryRun, "matrix-dry-run", false, "Print the matrix plan without executing builds")
	BuildCmd.Flags().BoolVar(&buildDebug, "debug", false, "Show verbose build output, commands, and Docker operations")
}

func validateName(name string) error {
	if name == "" {
		return phelixerr.New(phelixerr.CodeInvalidArgument, "application name cannot be empty")
	}
	return nil
}

func validateUniqueName(name string) error {
	for _, app := range app.Manager.(*app.AppManager).Apps {
		if app.Name == name {
			return phelixerr.Newf(
				phelixerr.CodeAlreadyExists,
				"application name %q is already in use",
				name,
			)
		}
	}
	return nil
}

func createAppEntry(id, name string, lang any) error {
	if appManager, ok := app.Manager.(*app.AppManager); ok {
		now := time.Now()
		currentDir, err := os.Getwd()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
		}

		langStr := "unknown"
		if langBuilt, ok := lang.(builder.Language); ok {
			langStr = string(langBuilt)
		} else if s, ok := lang.(string); ok {
			langStr = s
		}

		appManager.Apps[id] = &app.AppInfo{
			ID:          id,
			Name:        name,
			Status:      "initializing",
			BuildStatus: "building",
			CreatedAt:   now,
			UpdatedAt:   now,
			Directory:   currentDir,
			Language:    langStr,
		}
		return appManager.SaveState()
	}
	return phelixerr.New(phelixerr.CodeServer, "invalid app manager type")
}

// buildApplication runs the compile step and returns the captured build
// report for the successful build. On failure, it prints a concise post-mortem
// summary (never persists anything) and returns the error unchanged.
func buildApplication(id string, extraArgs []string, buildMgr *builder.BuildManager, outputPath string) (report *buildreport.Report, err error) {
	appInfo, err := GetAppInfo(id)
	if err != nil {
		return nil, err
	}

	projectRoot := appInfo.Directory

	lang := builder.ParseLanguage(appInfo.Language)
	if !lang.IsSupported() {
		lang = buildMgr.DetectLanguage(projectRoot)
	}

	progress := matrix.NewProgressBar(fmt.Sprintf("Build %s", id), os.Stdout)
	progress.Update("preparing", 0, 0)
	progress.Start()
	defer func() {
		if err != nil {
			progress.Finish("failed")
		}
	}()

	buildCtx, prepErr := buildMgr.PrepareBuild(
		id,
		appInfo.Name,
		projectRoot,
		outputPath,
		lang,
		extraArgs,
	)
	if prepErr != nil {
		printFailedBuildSummary(lang, nil)
		err = phelixerr.Wrap(phelixerr.CodeBuildFailed, "build preparation failed", prepErr)
		markBuildFailed(id)
		return nil, err
	}

	progress.Update(fmt.Sprintf("compiling %s", buildMgr.FormatLanguage(lang)), 0, 0)

	if err = buildMgr.ExecuteBuild(buildCtx); err != nil {
		printFailedBuildSummary(lang, buildCtx.Config)
		markBuildFailed(id)
		err = phelixerr.Wrap(phelixerr.CodeBuildFailed, "build failed", err)
		return nil, err
	}

	report = newNativeBuildReport(lang, buildCtx.Config, outputPath)

	progress.Finish("done")

	if appManager, ok := app.Manager.(*app.AppManager); ok {
		if appInfoPtr, exists := appManager.Apps[id]; exists {
			appInfoPtr.BuildStatus = "success"
			appInfoPtr.Language = string(lang)
			appInfoPtr.UpdatedAt = time.Now()
			_ = appManager.SaveState()
		}
	}

	return report, nil
}

func startApplication(id, name string) error {
	return startApplicationOnPort(id, name, buildPort)
}

func startApplicationOnPort(id, name string, port int) error {
	fmt.Printf("  %s Starting application on port %d...\n", color.BlueString("→"), port)
	if err := app.Manager.StartApplication(id, port, name); err != nil {
		return phelixerr.Wrap(
			phelixerr.CodeProcessFailed,
			fmt.Sprintf("failed to start application %q (ID: %s)", name, id),
			err,
		)
	}
	return nil
}

// runMatrixMode executes a matrix build from a normalized profile (the same
// representation CLI flags, the phelix.yaml matrix profile, and the wizard
// converge into): base Cartesian product → include → exclude.
func runMatrixMode(name string, projectRoot string, extraArgs []string, tag string, prof *matrix.Profile) error {
	plan, err := prof.Plan()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid matrix build plan", err)
	}

	fmt.Printf("%s Matrix build: %s (%d combinations)\n",
		color.BlueString("→"), color.CyanString(name), len(plan.Combinations))
	if plan.IncludedCount > 0 || plan.ExcludedCount > 0 {
		fmt.Printf("    %s base %d · included +%d · excluded -%d\n",
			color.New(color.Faint).Sprint("•"), plan.BaseCount, plan.IncludedCount, plan.ExcludedCount)
	}
	for _, c := range plan.Combinations {
		fmt.Printf("    %s %s\n", color.New(color.Faint).Sprint("•"), c.ID())
	}
	fmt.Println()

	if matrixDryRun {
		fmt.Printf("  %s Dry run — no builds executed\n", color.YellowString("Note:"))
		return nil
	}

	runID := matrix.NewUniqueRunID(time.Now())
	fmt.Printf("%s Matrix Run: %s\n", color.BlueString("→"), color.CyanString(string(runID)))
	machine.SetActiveOperation(string(runID))

	run := matrix.NewRun(runID, name, projectRoot, prof, time.Now())
	run.Config.BuildArgs = append([]string(nil), extraArgs...)
	run.InitCombinations(plan.Combinations)
	if serr := matrix.SaveRun(run); serr != nil {
		fmt.Printf("  %s Warning: could not record matrix run history: %v\n", color.YellowString("⚠"), serr)
	}

	_, interrupted, err := startMatrixSession(run, plan.Combinations, extraArgs, buildDebug)
	if err != nil {
		return err
	}

	completeMatrixSession(run, tag)

	switch {
	case interrupted:
		return phelixerr.Newf(
			phelixerr.CodeBuildFailed,
			"matrix run %s interrupted — continue it with 'phelix build --matrix --resume'",
			runID,
		)
	case run.Failed > 0:
		return phelixerr.Newf(
			phelixerr.CodeBuildFailed,
			"matrix run %s completed with %d failure(s) out of %d combinations — retry them with 'phelix matrix retry %s --failed'",
			runID, run.Failed, run.Total, runID,
		)
	}

	if machine.Active() {
		return writeEnvelopeResult(machine.Success(string(runID), matrixBuildResult{
			RunID:      string(runID),
			App:        name,
			Total:      run.Total,
			Succeeded:  run.Succeeded,
			Failed:     run.Failed,
			Skipped:    run.Skipped,
			ReportPath: matrix.ReportPathFor(projectRoot),
		}))
	}
	return nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// newMatrixComboReport assembles the per-combination build report from a
// live execution result (used by tests and the build-report integration).
func newMatrixComboReport(r matrix.Result) *buildreport.Report {
	rc := matrix.RunCombination{
		Toolchain:   string(r.Combination.Lang),
		Version:     r.Combination.Version,
		Platform:    r.Combination.Platform,
		CacheStatus: r.CacheStatus,
		Artifact:    r.Artifact,
	}
	report := newMatrixComboReportFromRun(rc)
	report.DurationMS = r.Duration.Milliseconds()
	return report
}

func markBuildFailed(id string) {
	appManager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return
	}

	info, exists := appManager.Apps[id]
	if !exists {
		return
	}

	info.BuildStatus = "failed"
	info.Status = "failed"
	info.UpdatedAt = time.Now()
	_ = appManager.SaveState()
}
