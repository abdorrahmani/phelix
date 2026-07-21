package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/abdorrahmani/phelix/cmd/auth"
	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
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
)

var BuildCmd = &cobra.Command{
	Use:   "build <NAME> --port <PORT>",
	Short: "Builds and runs an application (Go/Rust) with a specified name",
	Long:  "Compiles an application from the current directory (auto-detects language) with the given name and starts it immediately",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateSession(); err != nil {
		}

		name := args[0]
		if err := validateName(name); err != nil {
			return err
		}

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("%s Failed to load state: %w", color.RedString("✗"), err)
		}

		if err := validateUniqueName(name); err != nil {
			return err
		}

		// Get project root
		currentDir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("%s failed to get current directory: %v", color.RedString("✗"), err)
		}

		// Detect language
		buildMgr := builder.NewBuildManager()
		lang := buildMgr.DetectLanguage(currentDir)
		if !lang.IsSupported() {
			return fmt.Errorf("%s unsupported or unknown project language: %s", color.RedString("✗"), lang)
		}

		// Check toolchain; prompt to install if missing
		fmt.Printf("  %s Checking toolchain...\n", color.BlueString("→"))
		if err := toolchain.EnsureTool(lang, Confirm); err != nil {
			return fmt.Errorf("%s %v", color.RedString("✗"), err)
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

		if !noUpload {
			fmt.Printf("%s Uploading app information to server...\n", color.BlueString("→"))
			if err := auth.SendAppsToServer(); err != nil {
				log.Printf("%s Error sending apps to server: %v", color.YellowString("⚠"), err)
				fmt.Printf("%s Warning: Failed to send app information to server: %v\n", color.YellowString("⚠"), err)
			}
		} else {
			fmt.Printf("  %s Skipping upload to server (--no-upload was set)\n", color.YellowString("Note:"))
		}

		if err := startApplication(id, name); err != nil {
			return err
		}

		fmt.Printf("%s Application %s (ID: %s) started successfully on port %d\n",
			color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(id), buildPort)
		return nil
	},
}

func init() {
	BuildCmd.Flags().IntVarP(&buildPort, "port", "p", defaultPort, "Port to run the application on")
	BuildCmd.Flags().StringArrayVarP(&buildArgs, "build-arg", "a", nil, "Extra build argument to pass to the underlying build tool; can be provided multiple times")
	BuildCmd.Flags().BoolVar(&noUpload, "no-upload", false, "If set, do not upload/send app information to the server")
}

func validateSession() error {
	sessionFile := filepath.Join(os.Getenv("HOME"), ".phelix", "session.json")
	if _, err := os.Stat(sessionFile); os.IsNotExist(err) {
		return fmt.Errorf("⚠ Authentication required. Please run 'phelix auth' first")
	}

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return fmt.Errorf(" ⚠ error reading session file: %w", err)
	}

	var session struct {
		SessionID string    `json:"sessionID"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}

	if err := json.Unmarshal(data, &session); err != nil {
		return fmt.Errorf("⚠ error parsing session file: %w", err)
	}

	if time.Now().After(session.ExpiresAt) {
		return fmt.Errorf("⚠ session expired. Please run 'phelix auth' again")
	}

	return nil
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("⚠ application name cannot be empty")
	}
	return nil
}

func validateUniqueName(name string) error {
	for _, app := range app.Manager.(*app.AppManager).Apps {
		if app.Name == name {
			return fmt.Errorf("⚠ application name '%s' is already in use", name)
		}
	}
	return nil
}

func createAppEntry(id, name string, lang interface{}, noUpload bool) error {
	if appManager, ok := app.Manager.(*app.AppManager); ok {
		now := time.Now()
		currentDir, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("%s failed to get current directory: %v", color.RedString("✗"), err)
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
	return fmt.Errorf("%s invalid app manager type", color.RedString("✗"))
}

func buildApplication(id string, extraArgs []string, buildMgr *builder.BuildManager) error {
	outputPath := filepath.Join(".", fmt.Sprintf("app_%s", id))
	appInfo := app.Manager.(*app.AppManager).Apps[id]
	if appInfo == nil {
		return fmt.Errorf("%s application %s not found in state (it may have been removed by a concurrent operation); please retry", color.RedString("✗"), id)
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
		return fmt.Errorf("%s build preparation failed: %v", color.RedString("✗"), err)
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
		return fmt.Errorf("%s build failed: %v", color.RedString("✗"), err)
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
		return fmt.Errorf("%s failed to start application: %w", color.RedString("✗"), err)
	}
	return nil
}
