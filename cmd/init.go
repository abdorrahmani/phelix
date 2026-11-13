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
	initName           string
	initType           string
	initPort           int
	initRuntime        string
	initDockerBuild    string
	initComposeService string
	initYes            bool
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

		if initType == project.TypeWorker {
			cfg.Type = project.TypeWorker
			// A worker has no port and no endpoint probes; the supervision
			// defaults document themselves in the generated file.
			cfg.Port = 0
			cfg.Health = &project.HealthConfig{Mode: project.WorkerHealthProcess}
			cfg.Process = &project.ProcessConfig{}
			cfg.Deploy = &project.DeployConfig{
				Strategy: project.StrategyClassic,
				Replicas: 1,
			}
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

		if cfg.Type == project.TypeWorker {
			if cfg.Port != 0 {
				return phelixerr.New(phelixerr.CodeInvalidArgument,
					"--port is not valid with --type worker\nHint: workers do not listen on a port")
			}
			if err := project.Save(dir, cfg); err != nil {
				return err
			}
			if initHooksExample {
				if err := appendHooksExample(dir); err != nil {
					return err
				}
			}
			fmt.Printf("%s Created %s (name: %s, type: worker, replicas: %d)\n",
				color.GreenString("✓"), color.CyanString(project.FileName), cfg.Name, cfg.Deploy.Replicas)
			fmt.Printf("  %s supervision: restart policy %s with jittered backoff; deploy with %s.\n",
				color.BlueString("→"), project.DefaultRestartPolicy, color.CyanString("phelix build "+cfg.Name))
			return nil
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
			dockerCfg, derr := resolveInitDockerBuild(cmd, dir, cfg.Name)
			if derr != nil {
				return derr
			}
			cfg.Deploy.Docker = dockerCfg // nil leaves build unset (dockerfile default)
		}

		if err := project.Save(dir, cfg); err != nil {
			return err
		}
		if initHooksExample {
			if err := appendHooksExample(dir); err != nil {
				return err
			}
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
			if cfg.Deploy.Docker != nil && cfg.Deploy.Docker.Build == project.DockerBuildCompose {
				fmt.Printf("  %s image built from compose service %s (build only — runtime env stays with 'phelix env').\n",
					color.BlueString("→"), color.CyanString(cfg.Deploy.Docker.Service))
			}
			fmt.Printf("  %s run Phelix on the host (with a reachable Docker daemon), then deploy with %s.\n",
				color.BlueString("→"), color.CyanString("phelix rebuild "+cfg.Name))
		}
		return nil
	},
}

// resolveInitDockerBuild decides the docker image-build strategy for `phelix
// init`. The --docker-build / --compose-service flags win; otherwise, in a TTY
// (and not --yes) with a docker-compose.yml present, it offers build: compose
// (service defaults to the app name). Returns nil to leave build unset — the
// dockerfile default. Additive and fully skippable.
func resolveInitDockerBuild(cmd *cobra.Command, dir, appName string) (*project.DockerRuntimeConfig, error) {
	if cmd.Flags().Changed("docker-build") {
		switch initDockerBuild {
		case project.DockerBuildDockerfile, "":
			return nil, nil
		case project.DockerBuildCompose:
			svc := strings.TrimSpace(initComposeService)
			if svc == "" {
				svc = appName
			}
			return &project.DockerRuntimeConfig{Build: project.DockerBuildCompose, Service: svc}, nil
		default:
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"invalid --docker-build %q\nHint: expected one of: dockerfile, compose", initDockerBuild)
		}
	}
	if !IsInteractive() || initYes {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(dir, project.DefaultComposeFile)); err != nil {
		return nil, nil // no compose file — nothing to offer
	}
	use, err := PromptConfirm(
		"A "+project.DefaultComposeFile+" is present. Build the image from a compose service (keep compose as the build's source of truth)?", false)
	if err != nil || !use {
		return nil, nil
	}
	svc, err := PromptString("Compose service that builds this app", appName)
	if err != nil {
		return nil, nil
	}
	if svc = strings.TrimSpace(svc); svc == "" {
		svc = appName
	}
	return &project.DockerRuntimeConfig{Build: project.DockerBuildCompose, Service: svc}, nil
}

func init() {
	InitCmd.Flags().StringVar(&initName, "name", "", "Application name (defaults to the directory name)")
	InitCmd.Flags().StringVar(&initType, "type", "", "Workload type: service (default) or worker (a supervised background process with no port)")
	InitCmd.Flags().IntVar(&initPort, "port", 0, "Application port (interactive prompt when unset in a TTY)")
	InitCmd.Flags().StringVar(&initRuntime, "runtime", "", "Launch runtime: native (host process) or docker (container). Interactive prompt when unset in a TTY; defaults to native")
	InitCmd.Flags().StringVar(&initDockerBuild, "docker-build", "", "Docker image-build strategy (docker runtime only): dockerfile (default) or compose")
	InitCmd.Flags().StringVar(&initComposeService, "compose-service", "", "Compose service that builds this app's image (used with --docker-build compose; defaults to the app name)")
	InitCmd.Flags().BoolVar(&initYes, "yes", false, "Overwrite an existing phelix.yaml without prompting")
	InitCmd.Flags().BoolVar(&initHooksExample, "hooks-example", false, "Append a commented lifecycle hooks example to the generated phelix.yaml")
}

// inferProjectName uses the current directory name as the application name.
var initHooksExample bool

const hooksExample = `
# Deployment lifecycle hooks (optional). Every hook is an argument array, never
# a shell string. Timeouts, retries and failure policies are per hook.
#
# hooks:
#   pre_build:
#     command: ["./bin/migrate", "up"]
#     timeout: 60s
#     retries: 1
#     retry_interval: 2s
#     continue_on_error: false
#   post_build:
#     command: ["./scripts/validate-build.sh"]
#     timeout: 30s
#   pre_deploy:
#     command: ["./scripts/check-database.sh"]
#     timeout: 30s
#   post_start:
#     command: ["./bin/warm-cache"]
#     timeout: 30s
#   pre_traffic:
#     command: ["./scripts/smoke-test.sh"]
#     timeout: 60s
#   post_deploy:
#     command: ["./scripts/notify-deployment.sh"]
#     timeout: 15s
#     continue_on_error: true
#   pre_stop:
#     command: ["./scripts/graceful-shutdown.sh"]
#     timeout: 20s
#   post_rollback:
#     command: ["./scripts/notify-rollback.sh"]
#     timeout: 15s
#     continue_on_error: true
`

func appendHooksExample(dir string) error {
	path := filepath.Join(dir, project.FileName)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "open phelix.yaml", err)
	}
	defer file.Close()
	if _, err := file.WriteString(hooksExample); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "write hooks example", err)
	}
	return nil
}

func inferProjectName() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Base(dir)
}
