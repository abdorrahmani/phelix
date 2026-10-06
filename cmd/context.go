package cmd

// context.go — the composite Phase 2 snapshot: one bounded envelope combining
// the sections an agent needs before mutating anything. Every section comes
// from the same builders the targeted `inspect` subcommands use; the composite
// only sets bounds and reports truncation. Facts only — no recommendations.

import (
	"fmt"
	"os"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/spf13/cobra"
)

var (
	contextJSON       bool
	contextVersions   int
	contextOperations int
	contextLogLines   int
)

var ContextCmd = &cobra.Command{
	Use:   "context [ID|AppName]",
	Short: "One bounded, structured snapshot of the current Phelix environment",
	Long: "Composes project, runtime, configuration, capability and (when an app is " +
		"named) application, deployment, version, health, operation and log context " +
		"into a single machine-readable envelope. Facts only — decisions belong to " +
		"the caller. Use --json for the versioned machine envelope.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if contextJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

		versions, err := validateContextLimit("versions", contextVersions, contextDefaultVersions, contextMaxVersions)
		if err != nil {
			return err
		}
		operations, err := validateContextLimit("operations", contextOperations, contextDefaultOperations, contextMaxOperations)
		if err != nil {
			return err
		}
		logLines, err := validateContextLimit("log-lines", contextLogLines, contextDefaultLogLines, contextMaxLogLines)
		if err != nil {
			return err
		}

		result := &contextResult{}

		// Project / runtime / config / capabilities: always present — they
		// describe the environment the command runs in. A directory without a
		// detectable project is a fact reported by omitting the project and
		// config sections, not a failed command.
		dir, err := currentDir()
		if err != nil {
			return err
		}
		var notAProject error
		project, perr := buildProjectContext(dir)
		if perr != nil {
			notAProject = perr
		} else {
			result.Project = project
			cfg, cfgErr := projectLoadForContext(dir)
			if cfgErr != nil && phelixerr.CodeOf(cfgErr) != phelixerr.CodeNotFound {
				return cfgErr
			}
			result.Config = buildConfigContext(cfg)
		}
		result.Runtime = buildRuntimeContext()
		result.Capabilities = buildCapabilitiesContext()

		// App-scoped sections: only when an app was named. The composite must
		// not guess which app the caller means.
		if len(args) > 0 && args[0] != "" {
			appInfo, err := resolveContextAppInfo(args[0])
			if err != nil {
				return err
			}
			status, err := app.Manager.StatusApplication(appInfo.ID)
			if err != nil {
				return err
			}
			proxyUp, proxyByApp := proxySnapshot()
			result.Application = buildAppContext(status, proxyUp, proxyByApp)

			state, loadErr := deploy.Load(appInfo.Name)
			if loadErr != nil && !os.IsNotExist(loadErr) {
				return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load deploy state", loadErr)
			}
			// No deploy.json is a fact (classic management / nothing
			// deployed), not a failure.
			result.Deployment = buildDeploymentContext(state)

			vers, err := buildVersionsContext(appInfo.Name, versions)
			if err != nil {
				return err
			}
			result.Versions = vers
			if vers.Truncated {
				result.Truncated = true
			}

			healthView, err := buildHealthContext(appInfo.ID, appInfo.Name)
			if err != nil {
				return err
			}
			result.Health = healthView

			opsView, err := buildOperationsContext(appInfo.Name, operations)
			if err != nil {
				return err
			}
			result.Operations = opsView
			if opsView.Truncated {
				result.Truncated = true
			}

			logSpec := logViewSpec{Source: "app", Name: appInfo.Name, Path: appInfo.LogFile}
			if logSpec.Path == "" {
				logSpec.Path = logs.AppLogPath(appInfo.ID)
			}
			logsView, err := buildLogsContext(logSpec, logLines)
			if err != nil {
				// A missing log file is not a failed context: the app may
				// never have written one. Omit the section and note why on
				// the stderr diagnostics stream.
				logs.WarningFile("context", "logs section omitted for %s: %v", appInfo.Name, err)
			} else {
				result.Logs = logsView
				if logsView.Truncated {
					result.Truncated = true
				}
			}
		}

		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", result))
		}
		printContextSummary(result, dir, notAProject)
		return nil
	},
}

// resolveContextAppInfo loads app state and reconciles deployment reality
// before resolving the identifier to the underlying AppInfo (which carries
// the log-file path the status projection does not).
func resolveContextAppInfo(identifier string) (*app.AppInfo, error) {
	if err := app.Manager.LoadState(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}
	reconcileAppWithDeploy(identifier)
	return GetAppInfo(identifier)
}

func printContextSummary(result *contextResult, dir string, notAProject error) {
	if notAProject != nil {
		fmt.Printf("Project:   none detectable in %s (%s)\n", dir, phelixerr.Redact(notAProject.Error()))
	}
	if result.Project != nil {
		printProjectSummary(result.Project)
	}
	if result.Config != nil {
		printConfigSummary(result.Config)
	}
	if result.Runtime != nil {
		printRuntimeSummary(result.Runtime)
	}
	if result.Application != nil {
		printAppSummary(result.Application)
	}
	if result.Deployment != nil {
		printDeploymentSummary(result.Deployment)
	}
	if result.Versions != nil {
		printVersionsSummary(result.Versions)
	}
	if result.Health != nil {
		printHealthSummary(result.Health)
	}
	if result.Capabilities != nil {
		fmt.Printf("Capabilities: %s\n", joinCapabilities(result.Capabilities.Capabilities))
	}
	if result.Operations != nil {
		printOperationsSummary(result.Operations)
	}
	if result.Truncated {
		fmt.Println("Truncated: true (raise --versions/--operations/--log-lines for more)")
	}
}

func init() {
	ContextCmd.Flags().BoolVar(&contextJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	ContextCmd.Flags().IntVar(&contextVersions, "versions", 0, "Maximum versions in the snapshot (0 = default 20, max 100)")
	ContextCmd.Flags().IntVar(&contextOperations, "operations", 0, "Maximum operations in the snapshot (0 = default 20, max 100)")
	ContextCmd.Flags().IntVar(&contextLogLines, "log-lines", 0, "Maximum log lines in the snapshot (0 = default 100, max 1000)")
}
