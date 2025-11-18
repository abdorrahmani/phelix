package cmd

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/gophel/internal/app"
	"github.com/spf13/cobra"
)

var rebuildPort int

var RebuildCmd = &cobra.Command{
	Use:   "rebuild <ID|AppName> --port <PORT>",
	Short: "Rebuilds and runs a Go Application by its ID or AppName.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		identifier := args[0]

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("⚠ Failed to load state: %v", err)
		}

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			return err
		}

		name, portToUse := DetermineAppParameters(appInfo, cmd, rebuildPort)
		fmt.Printf("• Rebuilding application '%s' (ID: %s)\n", name, appInfo.ID)

		if err := stopExistingApp(appInfo); err != nil {
			return err
		}

		if err := rebuildApp(appInfo.ID); err != nil {
			return err
		}

		if err := app.Manager.StartApplication(appInfo.ID, portToUse, name); err != nil {
			return fmt.Errorf("⚠ Failed to start rebuilt application '%s' (ID: %s): %v", name, appInfo.ID, err)
		}

		fmt.Printf("✓ Application '%s' (ID: %s) rebuilt and started successfully on port %d\n", name, appInfo.ID, portToUse)
		return nil
	},
}

func init() {
	RebuildCmd.Flags().IntVarP(&rebuildPort, "port", "p", 8080, "Port to run the application on (defaults to previous port if unspecified)")
}

func stopExistingApp(appInfo *app.AppInfo) error {
	if appInfo.Status != "running" {
		fmt.Printf("⚠ Note: Application '%s' (ID: %s) was not running\n", appInfo.Name, appInfo.ID)
		return nil
	}

	if err := app.Manager.StopApplication(appInfo.ID); err != nil {
		return fmt.Errorf("⚠ Failed to stop application '%s' (ID: %s): %v", appInfo.Name, appInfo.ID, err)
	}
	return nil
}

func rebuildApp(id string) error {
	appInfo, err := GetAppInfo(id)
	if err != nil {
		return err
	}

	if appInfo.Directory == "" {
		return fmt.Errorf("⚠ Application directory not found for ID %s", id)
	}

	outputPath := filepath.Join(appInfo.Directory, fmt.Sprintf("app_%s", id))
	projectRoot := app.Manager.(*app.AppManager).Apps[id].Directory

	mainFile, err := FindMainFile(projectRoot)
	if err != nil {
		return fmt.Errorf("⚠ Failed to find main file: %v", err)
	}

	realPath, _ := filepath.Rel(projectRoot, mainFile)

	cmd := exec.Command("go", "build", "-o", outputPath, realPath)
	cmd.Dir = projectRoot

	if output, err := cmd.CombinedOutput(); err != nil {
		if appManager, ok := app.Manager.(*app.AppManager); ok {
			if info, exists := appManager.Apps[id]; exists {
				info.BuildStatus = "failed"
				info.UpdatedAt = time.Now()
				appManager.SaveState()
			}
		}
		return fmt.Errorf("⚠ Rebuild failed: %v\nOutput: %s", err, string(output))
	}

	if appManager, ok := app.Manager.(*app.AppManager); ok {
		if app, exists := appManager.Apps[id]; exists {
			app.BuildStatus = "success"
			app.UpdatedAt = time.Now()
			appManager.SaveState()
		}
	}
	return nil
}
