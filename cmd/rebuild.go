package cmd

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/toolchain"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var rebuildPort int
var rebuildArgs []string
var rebuildNoUpload bool

var RebuildCmd = &cobra.Command{
	Use:   "rebuild <ID|AppName> --port <PORT>",
	Short: "Rebuilds and runs a Go Application by its ID or AppName.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("%s Failed to load state: %v", color.RedString("✗"), err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		name, portToUse := DetermineAppParameters(appInfo, cmd, rebuildPort)

		// Detect and validate language
		buildMgr := builder.NewBuildManager()
		lang := builder.ParseLanguage(appInfo.Language)
		if !lang.IsSupported() {
			lang = buildMgr.DetectLanguage(appInfo.Directory)
		}

		if !lang.IsSupported() {
			return fmt.Errorf("%s unsupported or unknown project language: %s", color.RedString("✗"), lang)
		}

		// Check toolchain; prompt to install if missing
		fmt.Printf("  %s Checking toolchain...\n", color.BlueString("→"))
		if err := toolchain.EnsureTool(lang, Confirm); err != nil {
			return fmt.Errorf("%s %v", color.RedString("✗"), err)
		}

		fmt.Printf("%s Rebuilding application %s (ID: %s)\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID))
		fmt.Printf("  Language: %s\n", color.GreenString(buildMgr.FormatLanguage(lang)))

		// If user set no-upload flag, update the app info
		if rebuildNoUpload {
			appInfo.NoUpload = true
			_ = app.Manager.SaveState()
		}

		if err := stopExistingApp(appInfo); err != nil {
			return err
		}

		if err := rebuildApp(appInfo.ID, rebuildArgs, buildMgr); err != nil {
			return err
		}

		fmt.Printf("  %s Starting application on port %d...\n", color.BlueString("→"), portToUse)
		if err := app.Manager.StartApplication(appInfo.ID, portToUse, name); err != nil {
			return fmt.Errorf("%s Failed to start rebuilt application %s (ID: %s): %v", color.RedString("✗"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), err)
		}

		fmt.Printf("%s Application %s (ID: %s) rebuilt and started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), portToUse)
		return nil
	},
}

func init() {
	RebuildCmd.Flags().IntVarP(&rebuildPort, "port", "p", 8080, "Port to run the application on (defaults to previous port if unspecified)")
	RebuildCmd.Flags().StringArrayVarP(&rebuildArgs, "build-arg", "a", nil, "Extra build argument to pass to the underlying build tool; can be provided multiple times")
	RebuildCmd.Flags().BoolVar(&rebuildNoUpload, "no-upload", false, "If set, do not upload/send app information to the server after rebuild")
}

func stopExistingApp(appInfo *app.AppInfo) error {
	if appInfo.Status != "running" {
		fmt.Printf("  %s Note: Application %s (ID: %s) was not running\n", color.YellowString("⚠"), color.CyanString("'%s'", appInfo.Name), color.YellowString(appInfo.ID))
		return nil
	}

	fmt.Printf("  %s Stopping existing application...\n", color.BlueString("→"))
	if err := app.Manager.StopApplication(appInfo.ID); err != nil {
		return fmt.Errorf("%s Failed to stop application %s (ID: %s): %v", color.RedString("✗"), color.CyanString("'%s'", appInfo.Name), color.YellowString(appInfo.ID), err)
	}
	return nil
}

func rebuildApp(id string, extraArgs []string, buildMgr *builder.BuildManager) error {
	appInfo, err := GetAppInfo(id)
	if err != nil {
		return err
	}

	if appInfo.Directory == "" {
		return fmt.Errorf("%s Application directory not found for ID %s", color.RedString("✗"), id)
	}

	projectRoot := appInfo.Directory

	// Detect language
	lang := builder.ParseLanguage(appInfo.Language)
	if !lang.IsSupported() {
		lang = buildMgr.DetectLanguage(projectRoot)
	}

	if !lang.IsSupported() {
		return fmt.Errorf("%s unsupported or unknown project language: %s", color.RedString("✗"), lang)
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
		return fmt.Errorf("%s rebuild preparation failed: %v", color.RedString("✗"), err)
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
		return fmt.Errorf("%s rebuild failed: %v", color.RedString("✗"), err)
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
