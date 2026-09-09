package cmd

import (
	"errors"
	"fmt"
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
	"github.com/abdorrahmani/phelix/internal/matrix"
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
	noUpload  bool
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
)

var BuildCmd = &cobra.Command{
	Use:   "build [NAME] --port <PORT>",
	Short: "Builds and runs an application (Go/Rust) with a specified name",
	Long:  "Compiles an application from the current directory (auto-detects language) with the given name and starts it immediately",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Project configuration (phelix.yaml) supplies name/port defaults.
		// A missing file is fine; a present-but-invalid file fails fast here
		// so a broken config can never silently affect the build.
		projCfg, err := loadProjectConfig()
		if err != nil {
			return err
		}

		// --resume continues an interrupted matrix run. It implies matrix
		// mode and needs no application name — the run record carries it —
		// so it is handled before name resolution/prompting.
		if matrixResume != "" {
			return resumeMatrixRun(args, projCfg, buildArgs, buildTag, buildDebug)
		}

		if len(args) == 0 && projCfg != nil && projCfg.Name != "" {
			// phelix.yaml supplies the name; no prompt.
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

		// Precedence: CLI flag > phelix.yaml > default. When --port was not
		// passed, a phelix.yaml in the current directory supplies the port.
		// Matrix activity includes the phelix.yaml matrix profile (enabled
		// profiles activate the matrix without --matrix).
		isMatrix := matrixActive(cmd, projCfg)
		if !cmd.Flags().Changed("port") && !isMatrix {
			if projCfg != nil && projCfg.Port != 0 {
				buildPort = projCfg.Port
			}

			// When neither the flag nor the config supplied a port, offer to
			// choose it interactively (defaults to the configured or default
			// port). A config-supplied value is used as-is, without prompting.
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

		// Matrix builds compile artifacts for other platforms and never start
		// a local instance — port validation/availability is irrelevant and
		// must not block them (e.g. when the default port is already in use).
		if !isMatrix {
			if err := phelixport.Validate(buildPort); err != nil {
				return err
			}
			if err := phelixport.EnsureAvailable(buildPort); err != nil {
				return err
			}
		}

		// Build/run works without a session. Dashboard upload (metrics, events)
		// is skipped until the user logs in.
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

		if err := validateUniqueName(name); err != nil {
			return err
		}

		// Get project root
		currentDir, err := os.Getwd()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
		}

		// Detect language
		buildMgr := builder.NewBuildManager()
		lang := buildMgr.DetectLanguage(currentDir)
		if !lang.IsSupported() {
			return phelixerr.Newf(
				phelixerr.CodeUnsupportedProject,
				"unsupported or unknown project language: %s",
				lang,
			)
		}

		// --- Matrix build path ---
		// Active when --matrix is set, version/platform flags are provided,
		// or the phelix.yaml matrix profile is enabled. CLI flags, the YAML
		// profile, and the wizard all converge into one normalized profile
		// (matrix.Resolve) before the engine expands and executes it.
		if isMatrix {
			prof, rerr := resolveMatrixProfile(cmd, projCfg, lang)
			if rerr != nil {
				return rerr
			}
			return runMatrixMode(name, lang, currentDir, buildArgs, noUpload, buildTag, prof)
		}

		// --- Standard single-artifact build path ---
		// Check toolchain; prompt to install if missing
		fmt.Printf("  %s Checking toolchain...\n", color.BlueString("→"))
		if err := toolchain.EnsureTool(lang, Confirm); err != nil {
			return phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain check failed", err)
		}

		id := app.Manager.GenerateAppID()
		fmt.Printf("%s Building application %s (ID: %s)\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(id))
		fmt.Printf("  Language: %s\n", color.GreenString(buildMgr.FormatLanguage(lang)))

		if err := createAppEntry(id, name, lang, noUpload); err != nil {
			return err
		}

		// Apply health endpoints declared in phelix.yaml (desired state) to
		// the app's persisted health configuration before the app starts.
		if serr := syncProjectHealth(projCfg, id, name, buildPort); serr != nil {
			fmt.Printf("  %s Warning: could not apply health endpoints from %s: %v\n",
				color.YellowString("⚠"), project.FileName, serr)
		}

		binPath := filepath.Join(currentDir, fmt.Sprintf("app_%s", id))
		tracker, flushTelemetry := classicTracker(id, name, buildPort, 0)
		defer flushTelemetry()

		tracker.Building("classic build")
		report, err := buildApplication(id, buildArgs, buildMgr, binPath)
		if err != nil {
			tracker.Failed(err)
			return err
		}

		// --- Version recording ------------------------------------------------
		// The build succeeded. Record a new version with is_current=false.
		// The version becomes "current" only after the deploy (start) succeeds
		// and PromoteVersion is called below. This two-phase approach ensures
		// that a build which succeeds but whose start fails leaves the version
		// on disk for inspection but never becomes the active running instance.
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
			// Version recording is best-effort for the initial build path.
			// If it fails, the build still succeeded — we warn but don't abort.
			fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
		} else {
			fmt.Printf("  %s Recorded version v%d\n", color.BlueString("→"), rec.Version)
			tracker.SetTargetVersion(rec.Version)
		}

		// --- Build Report + regression analysis ---------------------------------
		// Pure observability: printed after recording; never affects success.
		emitBuildReport(name, report, gitCommit, rec, verErr)

		if !noUpload {
			err := auth.SendAppsToServer()
			switch {
			case errors.Is(err, auth.ErrNotLoggedIn):
				// Not logged in — the user was already told at the top of the
				// build that dashboard sync is skipped. Stay silent here so the
				// note isn't repeated.
			case err != nil:
				fmt.Printf("%s Warning: Failed to send app information to server: %v\n", color.YellowString("⚠"), err)
			default:
				fmt.Printf("%s App information uploaded to server\n", color.BlueString("→"))
			}
		} else {
			fmt.Printf("  %s Skipping upload to server (--no-upload was set)\n", color.YellowString("Note:"))
		}

		if err := startApplication(id, name); err != nil {
			// Deploy (start) failed. The version exists on disk in
			// builds/vN/ but is_current was never set to true and
			// PromoteVersion was never called, so the user can inspect
			// or retry without having a broken "current" pointer.
			tracker.Failed(err)
			return err
		}
		tracker.InstanceStarted("", classicPID(id), buildPort)

		// Deploy succeeded — promote the version so it becomes current.
		// This updates versions.json (is_current, deployed_at) and the
		// current → builds/vN symlink.
		if rec != nil {
			if err := deploy.PromoteVersion(name, rec.Version, "classic"); err != nil {
				fmt.Printf("  %s Warning: could not promote version: %v\n", color.YellowString("⚠"), err)
			}
		}
		tracker.PromoteCurrentVersion()
		tracker.Completed(fmt.Sprintf("running on port %d", buildPort))

		fmt.Printf("%s Application %s (ID: %s) started successfully on port %d\n",
			color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(id), buildPort)

		// Report build event
		phelixgrpc.ReportEvent(id, name, "build", true, "", 0, "classic", "")
		phelixgrpc.SendVersionListForApp(id, name, currentDir)
		return nil
	},
}

func init() {
	BuildCmd.Flags().IntVarP(&buildPort, "port", "p", defaultPort, "Port to run the application on")
	BuildCmd.Flags().StringArrayVarP(&buildArgs, "build-arg", "a", nil, "Extra build argument to pass to the underlying build tool; can be provided multiple times")
	BuildCmd.Flags().BoolVar(&noUpload, "no-upload", false, "If set, do not upload/send app information to the server")
	BuildCmd.Flags().StringVar(&buildTag, "tag", "", "Optional tag for this build (e.g. \"hotfix-auth-bug\"); stored as metadata alongside the auto-incremented version")

	// Matrix build flags.
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

func createAppEntry(id, name string, lang interface{}, noUpload bool) error {
	if appManager, ok := app.Manager.(*app.AppManager); ok {
		now := time.Now()
		currentDir, err := os.Getwd()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
		}

		// Convert lang to string
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
			NoUpload:    noUpload,
		}
		return appManager.SaveState()
	}
	return phelixerr.New(phelixerr.CodeServer, "invalid app manager type")
}

// buildApplication runs the compile step and returns the captured build
// report for the successful build. On failure it prints a concise post-mortem
// summary (never persists anything) and returns the error unchanged.
func buildApplication(id string, extraArgs []string, buildMgr *builder.BuildManager, outputPath string) (report *buildreport.Report, err error) {
	appInfo := app.Manager.(*app.AppManager).Apps[id]
	if appInfo == nil {
		return nil, phelixerr.Newf(
			phelixerr.CodeNotFound,
			"application %s not found in state (it may have been removed by a concurrent operation); please retry",
			id,
		)
	}
	projectRoot := appInfo.Directory

	// Detect language
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

	// Prepare build context
	buildCtx, prepErr := buildMgr.PrepareBuild(
		id,
		appInfo.Name,
		projectRoot,
		outputPath,
		lang,
		extraArgs,
		appInfo.NoUpload,
	)
	if prepErr != nil {
		printFailedBuildSummary(lang, nil)
		err = phelixerr.Wrap(phelixerr.CodeBuildFailed, "build preparation failed", prepErr)
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if app, exists := appManager.Apps[id]; exists {
				app.BuildStatus = "failed"
				app.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}
		return nil, err
	}

	progress.Update(fmt.Sprintf("compiling %s", buildMgr.FormatLanguage(lang)), 0, 0)
	// Execute build
	if err = buildMgr.ExecuteBuild(buildCtx); err != nil {
		// Failed builds are never recorded as versions; print post-mortem.
		printFailedBuildSummary(lang, buildCtx.Config)
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if appInfo, exists := appManager.Apps[id]; exists {
				appInfo.BuildStatus = "failed"
				appInfo.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}
		err = phelixerr.Wrap(phelixerr.CodeBuildFailed, "build failed", err)
		return nil, err
	}

	report = newNativeBuildReport(lang, buildCtx.Config, outputPath)

	progress.Finish("done")

	// Update build status
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
//
// Every execution gets a Matrix Run ID; the run (configuration snapshot +
// per-combination results) is persisted to the local run history before the
// first build starts and updated as each combination completes, so an
// interrupted run can be resumed with --resume (`phelix build --matrix
// --resume`) without rebuilding completed combinations.
//
// Build phase uses fail-open semantics: if one combination fails, we continue
// building the rest. This is deliberate — a matrix build produces multiple
// artifacts and users need to know which combinations are broken, not just that
// "something failed". The full summary at the end shows exactly what succeeded
// and what failed, with error details per failure.
func runMatrixMode(name string, lang builder.Language, projectRoot string, extraArgs []string, noUpload bool, tag string, prof *matrix.Profile) error {
	// Expand and validate the plan — the same Expand engine (base product →
	// include → exclude) every matrix path uses.
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

	// Every matrix execution is identified by a Matrix Run; the run snapshot
	// (effective configuration at execution time) is taken before building,
	// persisted immediately, and updated as combinations complete.
	runID := matrix.NewUniqueRunID(time.Now())
	fmt.Printf("%s Matrix Run: %s\n", color.BlueString("→"), color.CyanString(string(runID)))

	run := matrix.NewRun(runID, name, projectRoot, prof, time.Now())
	run.Config.BuildArgs = append([]string(nil), extraArgs...)
	run.InitCombinations(plan.Combinations)
	if serr := matrix.SaveRun(run); serr != nil {
		fmt.Printf("  %s Warning: could not record matrix run history: %v\n", color.YellowString("⚠"), serr)
	}

	executed, interrupted, err := startMatrixSession(run, plan.Combinations, extraArgs, buildDebug)
	if err != nil {
		return err
	}

	// Report (terminal + JSON) and version recording for the whole run.
	completeMatrixSession(run, executed, tag)

	// Return an error when combinations failed or the run was interrupted, so
	// the CLI exit code is non-zero. The user sees the full report above —
	// this just ensures scripts can detect failures.
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
