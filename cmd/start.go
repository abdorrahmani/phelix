package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/buildreport"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixgrpc "github.com/abdorrahmani/phelix/internal/grpc"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	port        int
	startEnsure bool
)

var StartCmd = &cobra.Command{
	Use:   "start [ID|AppName] --port <PORT>",
	Short: "Starts an application by ID or AppName. With no argument it prompts which app (or all) to start.",
	Long:  "Starts an existing application. With --ensure, it becomes idempotent: build the app if it does not exist, start it if stopped, and rebuild/start if startup fails.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := app.Manager.LoadState(); err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
		}

		// Interactive: when no app is named but stdin is a TTY, let the user
		// pick one app or "All applications". Non-TTY keeps the original
		// start-all behavior so scripts are unaffected.
		if len(args) == 0 && IsInteractive() {
			chosen, err := PromptApp(true, "Select application to start")
			if err != nil {
				return err
			}
			// PromptApp returns "" when "All applications" is selected.
			if chosen != "" {
				args = []string{chosen}
			}
		}

		// If no ID or AppName provided, start all apps
		if len(args) == 0 {
			apps := app.Manager.ListApplications()
			if len(apps) == 0 {
				fmt.Println("No applications found to start.")
				return nil
			}

			fmt.Println(color.BlueString("→") + " Starting all applications...")
			var failed []string
			for _, a := range apps {
				if a.Status == "running" {
					fmt.Printf("  %s %s (ID: %s) is already running\n", color.GreenString("✓"), color.CyanString("'%s'", a.Name), color.YellowString(a.ID))
					continue
				}

				// Zero-downtime deployments restore through their own path so
				// the public port stays proxy-owned (never classic-bound).
				if loadDeployState(a.Name) != nil {
					info, gerr := GetAppInfo(a.ID)
					if gerr == nil {
						if serr := runDeployAwareStart(info); serr != nil {
							fmt.Printf("    %s Failed to start %s (ID: %s): %v\n", color.RedString("✗"), color.CyanString("'%s'", a.Name), color.YellowString(a.ID), serr)
							failed = append(failed, fmt.Sprintf("%s: %v", a.Name, serr))
						} else {
							phelixgrpc.ReportEvent(a.ID, a.Name, "start", true, "", 0, "", "")
						}
						continue
					}
				}

				fmt.Printf("  %s Starting %s (ID: %s) on port %d...\n", color.BlueString("→"), color.CyanString("'%s'", a.Name), color.YellowString(a.ID), a.Port)
				if err := app.Manager.StartApplication(a.ID, a.Port, a.Name); err != nil {
					// Collect per-app failures and return a single aggregated
					// error at the end so the CLI renders one error block.
					fmt.Printf("    %s Failed to start %s (ID: %s): %v\n", color.RedString("✗"), color.CyanString("'%s'", a.Name), color.YellowString(a.ID), err)
					failed = append(failed, fmt.Sprintf("%s: %v", a.Name, err))
					continue
				}
				fmt.Printf("    %s %s started successfully\n", color.GreenString("✓"), color.CyanString("'%s'", a.Name))
				phelixgrpc.ReportEvent(a.ID, a.Name, "start", true, "", 0, "", "")
			}
			if len(failed) > 0 {
				return phelixerr.Newf(
					phelixerr.CodeProcessFailed,
					"failed to start %d application(s)\n%s",
					len(failed),
					strings.Join(failed, "\n"),
				)
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

		// Zero-downtime deployments restore through their own path: relaunch
		// the active slot from its recorded binary and re-establish the proxy
		// route. Launching a classic process on the public port would fight
		// the proxy for ownership.
		if loadDeployState(name) != nil {
			return runDeployAwareStart(appInfo)
		}

		fmt.Printf("%s Starting application %s (ID: %s) on port %d\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), usePort)

		if err := app.Manager.StartApplication(appInfo.ID, usePort, name); err != nil {
			if !startEnsure {
				return phelixerr.Wrap(
					phelixerr.CodeProcessFailed,
					fmt.Sprintf("failed to start application %q (ID: %s)", name, appInfo.ID),
					err,
				)
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

			if _, rerr := rebuildApp(appInfo.ID, []string{}, buildMgr, ""); rerr != nil {
				return rerr
			}

			if err := app.Manager.StartApplication(appInfo.ID, usePort, name); err != nil {
				return phelixerr.Wrap(
					phelixerr.CodeProcessFailed,
					fmt.Sprintf("failed to start rebuilt application %q (ID: %s)", name, appInfo.ID),
					err,
				)
			}

			fmt.Printf("%s Application %s (ID: %s) rebuilt and started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), usePort)
			return nil
		}

		fmt.Printf("%s Application %s (ID: %s) started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(appInfo.ID), usePort)
		phelixgrpc.ReportEvent(appInfo.ID, name, "start", true, "", 0, "", "")
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
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
	}

	// Detect language
	buildMgr := builder.NewBuildManager()
	lang := buildMgr.DetectLanguage(cwd)
	if !lang.IsSupported() {
		return phelixerr.Newf(
			phelixerr.CodeUnsupportedProject,
			"unsupported or unknown project language: %s",
			lang,
		)
	}

	// Validate tools
	if err := buildMgr.ValidateTools(lang); err != nil {
		return phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain validation failed", err)
	}

	fmt.Printf("%s Application %s does not exist. Building it with ID: %s\n", color.BlueString("→"), color.CyanString("'%s'", name), color.YellowString(id))
	fmt.Printf("  Language: %s\n", color.GreenString(buildMgr.FormatLanguage(lang)))

	if err := createAppEntry(id, name, lang, noUpload); err != nil {
		return err
	}

	binPath := filepath.Join(cwd, fmt.Sprintf("app_%s", id))
	report, berr := buildApplication(id, []string{}, buildMgr, binPath)
	if berr != nil {
		return berr
	}

	if err := startApplicationOnPort(id, name, port); err != nil {
		return err
	}

	// Build report only: this legacy ensure-path does not go through version
	// recording, so no regression analysis is possible (and none is required).
	if report != nil {
		buildreport.PrintReport(os.Stdout, buildreport.ReportView{
			AppName: name,
			Commit:  deploy.DetectGitCommit(cwd),
			Report:  report,
		})
	}

	fmt.Printf("%s Application %s (ID: %s) built and started successfully on port %d\n", color.GreenString("✓"), color.CyanString("'%s'", name), color.YellowString(id), port)
	return nil
}
