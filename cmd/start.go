package cmd

import (
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	port        int
	startEnsure bool
)

var StartCmd = &cobra.Command{
	Use:   "start <ID|AppName> --port <PORT>",
	Short: "Starts an application by ID or AppName. If no argument is provided, starts all applications.",
	Long:  "Starts an existing application. With --ensure, it becomes idempotent: build the app if it does not exist, start it if stopped, and rebuild/start if startup fails.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {

		if err := app.Manager.LoadState(); err != nil {
			return fmt.Errorf("⚠ Failed to load state: %v", err)
		}

		// If no ID or AppName provided, start all apps
		if len(args) == 0 {
			apps := app.Manager.ListApplications()
			if len(apps) == 0 {
				fmt.Println("No applications found to start.")
				return nil
			}

			fmt.Println(color.BlueString("→") + " Starting all applications...")
			for _, a := range apps {
				if a.Status == "running" {
					fmt.Printf("  %s %s (ID: %s) is already running\n", color.GreenString("✓"), color.CyanString("'%s'", a.Name), color.YellowString(a.ID))
					continue
				}

				fmt.Printf("  %s Starting %s (ID: %s) on port %d...\n", color.BlueString("→"), color.CyanString("'%s'", a.Name), color.YellowString(a.ID), a.Port)
				if err := app.Manager.StartApplication(a.ID, a.Port, a.Name); err != nil {
					fmt.Printf("    %s Failed to start %s (ID: %s): %v\n", color.RedString("✗"), color.CyanString("'%s'", a.Name), color.YellowString(a.ID), err)
					continue
				}
				fmt.Printf("    %s %s started successfully\n", color.GreenString("✓"), color.CyanString("'%s'", a.Name))
			}
			return nil
		}

		// Start specific app
		identifier := args[0]

		appInfo, err := GetAppInfo(identifier)
		if err != nil {
			if startEnsure {
				return ensureNewApplication(identifier, port)
			}
			return err
		}

		name, usePort := DetermineAppParameters(appInfo, cmd, port)

		if startEnsure && appInfo.Status == "running" {
			fmt.Printf("%s Application %s (ID: %s) is already running on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), appInfo.Port)
			return nil
		}

		fmt.Printf("%s Starting application %s (ID: %s) on port %d\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), usePort)

		if err := app.Manager.StartApplication(appInfo.ID, usePort, name); err != nil {
			if !startEnsure {
				return fmt.Errorf("%s Failed to start application %s (ID: %s): %v", color.RedString("✗"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), err)
			}

			fmt.Printf("%s Start failed for application %s (ID: %s): %v\n", color.YellowString("⚠"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), err)

			// Try to rebuild
			buildMgr := builder.NewBuildManager()
			lang := builder.ParseLanguage(appInfo.Language)
			if !lang.IsSupported() {
				lang = buildMgr.DetectLanguage(appInfo.Directory)
			}

			fmt.Printf("%s Rebuilding application %s (ID: %s) with %s...\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), color.GreenString(buildMgr.FormatLanguage(lang)))

			if err := stopExistingApp(appInfo); err != nil {
				return err
			}

			if err := rebuildApp(appInfo.ID, []string{}, buildMgr); err != nil {
				return err
			}

			if err := app.Manager.StartApplication(appInfo.ID, usePort, name); err != nil {
				return fmt.Errorf("%s Failed to start rebuilt application %s (ID: %s): %v", color.RedString("✗"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), err)
			}

			fmt.Printf("%s Application %s (ID: %s) rebuilt and started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), usePort)
			return nil
		}

		fmt.Printf("%s Application %s (ID: %s) started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), usePort)
		return nil
	},
}

func init() {
	StartCmd.Flags().IntVarP(&port, "port", "p", 8080, "Port to run the application on")
	StartCmd.Flags().BoolVar(&startEnsure, "ensure", false, "Build if missing and rebuild if startup fails")
}

func ensureNewApplication(name string, port int) error {
	if err := validateName(name); err != nil {
		return err
	}

	if err := validateUniqueName(name); err != nil {
		return err
	}

	id := app.Manager.GenerateAppID()

	// Get current directory
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("%s failed to get current directory: %v", color.RedString("✗"), err)
	}

	// Detect language
	buildMgr := builder.NewBuildManager()
	lang := buildMgr.DetectLanguage(cwd)
	if !lang.IsSupported() {
		return fmt.Errorf("%s unsupported or unknown project language: %s", color.RedString("✗"), lang)
	}

	// Validate tools
	if err := buildMgr.ValidateTools(lang); err != nil {
		return fmt.Errorf("%s %v", color.RedString("✗"), err)
	}

	fmt.Printf("%s Application %s does not exist. Building it with ID: %s\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(id))
	fmt.Printf("  Language: %s\n", color.GreenString(buildMgr.FormatLanguage(lang)))

	if err := createAppEntry(id, name, lang, noUpload); err != nil {
		return err
	}

	if err := buildApplication(id, []string{}, buildMgr); err != nil {
		return err
	}

	if err := startApplicationOnPort(id, name, port); err != nil {
		return err
	}

	fmt.Printf("%s Application %s (ID: %s) built and started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(id), port)
	return nil
}
