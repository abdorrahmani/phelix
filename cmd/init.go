package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	initName    string
	initPort    int
	initRuntime string
	initYes     bool
)

// defaultInitPort mirrors the CLI's existing default port so init never has to
// invent a value.
const defaultInitPort = 8080

var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initializes phelix.yaml for the current project",
	Long:  "Detects the project type (Go/Rust) and creates a project-level phelix.yaml with the application name and port. Never modifies application source code.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := os.Getwd()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
		}

		fmt.Printf("%s Detecting project\n", color.BlueString("→"))

		buildMgr := builder.NewBuildManager()
		lang := buildMgr.DetectLanguage(dir)
		if !lang.IsSupported() {
			return phelixerr.Newf(
				phelixerr.CodeUnsupportedProject,
				"unsupported or unknown project language: %s",
				lang,
			)
		}
		fmt.Printf("  %s %s project detected\n", color.GreenString("✓"), builder.Language(lang))

		if project.Exists(dir) && !initYes {
			existing, err := project.Load(dir)
			if err == nil {
				fmt.Printf("  %s %s already exists (name: %s, port: %d)\n",
					color.YellowString("⚠"), project.FileName, existing.Name, existing.Port)
				fmt.Printf("  %s Use --yes to overwrite\n", color.YellowString("Note:"))
				return nil
			}
			// Malformed existing file — overwrite path below fixes it.
		}

		cfg := &project.Config{
			Name: initName,
			Port: initPort,
			// Watching defaults to disabled: a fresh project must not send
			// monitoring data to the backend until the user opts in (via this
			// file or `phelix watch`).
			Watching: project.WatchingDisable,
			// Generate a complete, working example: one default health
			// endpoint and the classic strategy. Every generated field is
			// supported and validated by project.Load.
			Health: &project.HealthConfig{
				Endpoints: []project.HealthEndpoint{
					{Name: "default", Path: "/health", Interval: "10s", Retries: 3, Mode: "auto"},
				},
			},
			Deploy: &project.DeployConfig{Strategy: project.StrategyClassic},
		}

		if cfg.Name == "" {
			cfg.Name = inferProjectName()
		}
		if IsInteractive() && cfg.Name == "" {
			entered, err := PromptString("Application name", "")
			if err != nil {
				return err
			}
			cfg.Name = entered
		}
		if err := validateName(cfg.Name); err != nil {
			return err
		}

		if cfg.Port == 0 {
			cfg.Port = defaultInitPort
		}
		if IsInteractive() && !cmd.Flags().Changed("port") && !initYes {
			chosen, err := PromptInt("Application port", cfg.Port)
			if err != nil {
				return err
			}
			cfg.Port = chosen
		}
		if err := phelixport.Validate(cfg.Port); err != nil {
			return err
		}

		runtime := initRuntime
		if runtime == "" && IsInteractive() && !initYes {
			picked, err := PromptSelect(
				"Launch runtime — how should this app's instances run?",
				[]string{
					project.RuntimeNative + "  (host process — the model that runs on a bare VPS)",
					project.RuntimeDocker + "  (container — one Phelix agent on the host manages many app containers)",
				},
			)
			if err != nil {
				return err
			}
			// The label carries a description after the value; keep the value.
			runtime = strings.Fields(picked)[0]
		}
		if runtime == "" {
			runtime = project.RuntimeNative
		}
		if runtime != project.RuntimeNative && runtime != project.RuntimeDocker {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"invalid runtime %q\nHint: expected one of: native, docker", runtime)
		}
		cfg.Deploy.Runtime = runtime

		if runtime == project.RuntimeDocker {
			cfg.Deploy.Strategy = project.StrategyBlueGreen
		}

		if err := project.Save(dir, cfg); err != nil {
			return err
		}

		// Diagnostic-only warning: a hardcoded port is not fatal here; doctor
		// gives the full report.
		hits, _ := project.ScanHardcodedPort(dir)
		for _, h := range hits {
			fmt.Printf("  %s Hardcoded listen port :%d found in %s:%d — applications should read PORT instead (run 'phelix doctor')\n",
				color.YellowString("⚠"), h.Port, h.File, h.Line)
		}

		fmt.Printf("%s Created %s (name: %s, port: %d, runtime: %s)\n",
			color.GreenString("✓"), color.CyanString(project.FileName), cfg.Name, cfg.Port, cfg.Deploy.Runtime)
		if cfg.Deploy.Runtime == project.RuntimeDocker {
			fmt.Printf("  %s docker runtime: each instance runs as a container; strategy set to %s (classic is native-only).\n",
				color.BlueString("→"), cfg.Deploy.Strategy)
			fmt.Printf("  %s run Phelix on the host (with a reachable Docker daemon), then deploy with %s.\n",
				color.BlueString("→"), color.CyanString("phelix rebuild "+cfg.Name))
		}
		return nil
	},
}

func init() {
	InitCmd.Flags().StringVar(&initName, "name", "", "Application name (defaults to the directory name)")
	InitCmd.Flags().IntVar(&initPort, "port", 0, "Application port (interactive prompt when unset in a TTY)")
	InitCmd.Flags().StringVar(&initRuntime, "runtime", "", "Launch runtime: native (host process) or docker (container). Interactive prompt when unset in a TTY; defaults to native")
	InitCmd.Flags().BoolVar(&initYes, "yes", false, "Overwrite an existing phelix.yaml without prompting")
}

// inferProjectName uses the current directory name as the application name.
func inferProjectName() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Base(dir)
}
