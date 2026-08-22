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
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/abdorrahmani/phelix/internal/matrix"
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
	buildDebug        bool
)

var BuildCmd = &cobra.Command{
	Use:   "build [NAME] --port <PORT>",
	Short: "Builds and runs an application (Go/Rust) with a specified name",
	Long:  "Compiles an application from the current directory (auto-detects language) with the given name and starts it immediately",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			if !IsInteractive() {
				return phelixerr.Newf(phelixerr.CodeInvalidArgument, "missing required <NAME>; usage: phelix build <NAME> --port <PORT>")
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

		// When the port flag was not passed on the command line, offer to
		// choose it interactively (defaults to the configured port).
		if IsInteractive() && !cmd.Flags().Changed("port") {
			chosen, err := PromptInt("Port to run the application on", buildPort)
			if err != nil {
				return err
			}
			buildPort = chosen
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
		// When --matrix is set (or --go-versions / --rust-versions / --platforms
		// are provided), we expand the cross product of versions × platforms
		// and build each combination concurrently via a bounded worker pool.
		if matrix.IsMatrixMode(matrixFlag, goVersions, rustVersions, platforms) {
			return runMatrixMode(name, lang, currentDir, buildArgs, noUpload, buildTag)
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

		if err := buildApplication(id, buildArgs, buildMgr); err != nil {
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
			filepath.Join(currentDir, fmt.Sprintf("app_%s", id)),
			gitCommit, buildTag,
			deploy.DefaultRetention{Max: 5}, logger,
		)
		if verErr != nil {
			// Version recording is best-effort for the initial build path.
			// If it fails, the build still succeeded — we warn but don't abort.
			fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
		} else {
			fmt.Printf("  %s Recorded version v%d\n", color.BlueString("→"), rec.Version)
		}

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
			return err
		}

		// Deploy succeeded — promote the version so it becomes current.
		// This updates versions.json (is_current, deployed_at) and the
		// current → builds/vN symlink.
		if rec != nil {
			if err := deploy.PromoteVersion(name, rec.Version, "classic"); err != nil {
				fmt.Printf("  %s Warning: could not promote version: %v\n", color.YellowString("⚠"), err)
			}
		}

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

func buildApplication(id string, extraArgs []string, buildMgr *builder.BuildManager) error {
	outputPath := filepath.Join(".", fmt.Sprintf("app_%s", id))
	appInfo := app.Manager.(*app.AppManager).Apps[id]
	if appInfo == nil {
		return phelixerr.Newf(
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
			if app, exists := appManager.Apps[id]; exists {
				app.BuildStatus = "failed"
				app.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}
		return phelixerr.Wrap(phelixerr.CodeBuildFailed, "build preparation failed", err)
	}

	fmt.Printf("  %s Building with %s...\n", color.BlueString("→"), color.GreenString(buildMgr.FormatLanguage(lang)))

	// Execute build
	if err := buildMgr.ExecuteBuild(buildCtx); err != nil {
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if appInfo, exists := appManager.Apps[id]; exists {
				appInfo.BuildStatus = "failed"
				appInfo.UpdatedAt = time.Now()
				_ = appManager.SaveState()
			}
		}
		return phelixerr.Wrap(phelixerr.CodeBuildFailed, "build failed", err)
	}

	duration := buildMgr.GetBuildDuration(buildCtx)
	fmt.Printf("  %s Build completed in %s\n", color.GreenString("✓"), color.YellowString(duration.String()))

	// Update build status
	if appManager, ok := app.Manager.(*app.AppManager); ok {
		if appInfoPtr, exists := appManager.Apps[id]; exists {
			appInfoPtr.BuildStatus = "success"
			appInfoPtr.Language = string(lang)
			appInfoPtr.UpdatedAt = time.Now()
			_ = appManager.SaveState()
		}
	}

	return nil
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

// runMatrixMode executes a matrix build: cross product of {toolchain version} × {platform}.
//
// Build phase uses fail-open semantics: if one combination fails, we continue
// building the rest. This is deliberate — a matrix build produces multiple
// artifacts and users need to know which combinations are broken, not just that
// "something failed". The full summary at the end shows exactly what succeeded
// and what failed, with error details per failure.
//
// After building, we record all successful artifacts in versions.json (additive
// schema) and write a report (JSON + terminal summary).
func runMatrixMode(name string, lang builder.Language, projectRoot string, extraArgs []string, noUpload bool, tag string) error {
	// Determine which version lists to use based on the detected language.
	versions := goVersions
	if lang == builder.Rust {
		versions = rustVersions
	}
	if len(versions) == 0 {
		return phelixerr.Newf(
			phelixerr.CodeInvalidArgument,
			"matrix mode requires version flags: use --go-versions or --rust-versions for %s projects",
			lang,
		)
	}

	// Parse the build plan.
	plan, err := matrix.ParsePlan(lang, versions, platforms)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid matrix build plan", err)
	}

	fmt.Printf("%s Matrix build: %s (%d combinations)\n",
		color.BlueString("→"), color.CyanString(name), len(plan.Combinations))
	for _, c := range plan.Combinations {
		fmt.Printf("    %s %s\n", color.New(color.Faint).Sprint("•"), c.ID())
	}
	fmt.Println()

	if matrixDryRun {
		fmt.Printf("  %s Dry run — no builds executed\n", color.YellowString("Note:"))
		return nil
	}

	// Build the appropriate builder.
	var buildFn matrix.BuildFunc
	switch lang {
	case builder.Go:
		gb := &matrix.GoMatrixBuilder{
			ProjectRoot: projectRoot,
			AppName:     name,
			UseDocker:   len(versions) > 1, // use Docker when multiple versions
			Debug:       buildDebug,
		}
		buildFn = gb.Build
	case builder.Rust:
		rb := &matrix.RustMatrixBuilder{ProjectRoot: projectRoot, AppName: name, Debug: buildDebug}
		buildFn = rb.Build
	default:
		return phelixerr.Newf(
			phelixerr.CodeUnsupportedProject,
			"unsupported language for matrix build: %s",
			lang,
		)
	}

	// Execute with bounded concurrency.
	startTime := time.Now()
	results := matrix.Execute(plan, buildFn, matrix.ExecutorConfig{
		Concurrency: matrixConcurrency,
		Debug:       buildDebug,
	})

	// Generate and display the report.
	report := matrix.GenerateReport(name, results, startTime)
	report.PrintTerminal()

	reportPath, _ := report.WriteJSON(projectRoot)
	if reportPath != "" {
		fmt.Printf("  %s Report written to %s\n", color.GreenString("✓"), reportPath)
	}

	// Record successful artifacts in the versioning system.
	if report.Succeeded > 0 {
		artifacts := make([]deploy.MatrixArtifact, 0, report.Succeeded)
		for _, r := range results {
			if r.Status != "success" {
				continue
			}
			artifacts = append(artifacts, deploy.MatrixArtifact{
				Platform:  r.Combination.Platform,
				Version:   r.Combination.Version,
				Binary:    r.Artifact,
				Status:    r.Status,
				SizeBytes: fileSize(r.Artifact),
			})
		}

		gitCommit := deploy.DetectGitCommit(projectRoot)
		logger := &colorLogger{}
		_, verErr := deploy.RecordMatrixBuild(
			name, tag, gitCommit, artifacts, "",
			deploy.DefaultRetention{Max: 5}, logger,
		)
		if verErr != nil {
			fmt.Printf("  %s Warning: could not record version: %v\n", color.YellowString("⚠"), verErr)
		} else {
			fmt.Printf("  %s Matrix build recorded in version history\n", color.GreenString("✓"))
		}
	}

	// Return an error if any combination failed, so the CLI exit code is non-zero.
	// The user sees the full report above — this just ensures scripts can detect failures.
	if report.Failed > 0 {
		return phelixerr.Newf(
			phelixerr.CodeBuildFailed,
			"matrix build completed with %d failure(s) out of %d combinations",
			report.Failed, report.Total,
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
