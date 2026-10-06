package cmd

// inspect.go — Phase 2 targeted inspection commands. Each subcommand answers
// ONE question about project, runtime, configuration, application,
// deployment, version, health, capability or operation state through the
// Phase 1 machine contract. Builders live in context_model.go; the composite
// snapshot lives in context.go.

import (
	"fmt"
	"os"
	"strings"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/proxy"
	"github.com/spf13/cobra"
)

// currentDir wraps os.Getwd with the standard structured error.
func currentDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
	}
	return dir, nil
}

// projectLoadForContext loads phelix.yaml for the config section; a missing
// file is returned as-is so the caller can render the defaults-only view.
func projectLoadForContext(dir string) (*project.Config, error) {
	return project.Load(dir)
}

func joinCapabilities(caps []string) string {
	if len(caps) == 0 {
		return "(none)"
	}
	return strings.Join(caps, ", ")
}

var InspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Inspect project, runtime, application and deployment state (machine-readable)",
	Long: "Targeted, bounded, structured inspection of the current Phelix environment.\n" +
		"Use --json for the versioned machine envelope (docs/reference/machine-contract.md).",
}

// resolveInspectApp loads app state, reconciles deployment reality (the same
// path `phelix status` uses) and resolves the identifier. Missing args fail
// deterministically in non-interactive sessions.
func resolveInspectApp(identifier string) (*app.AppStatus, error) {
	if err := app.Manager.LoadState(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}
	if identifier == "" {
		if !IsInteractive() {
			return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "missing required <ID|AppName>; usage: phelix inspect <topic> <ID|AppName>")
		}
		chosen, err := PromptApp(false, "Select application")
		if err != nil {
			return nil, err
		}
		identifier = chosen
	}
	reconcileAppWithDeploy(identifier)
	status, err := app.Manager.StatusApplication(identifier)
	if err != nil {
		// StatusApplication already carries the precise code (NOT_FOUND for a
		// missing app) — re-wrapping would mask it.
		return nil, err
	}
	return &status, nil
}

// proxySnapshot loads the proxy daemon snapshot (same helper `phelix list`
// uses; failures are silent — context works without the proxy).
func proxySnapshot() (bool, map[string]proxy.AppStatus) {
	return loadProxySnapshot()
}

// --- inspect project -----------------------------------------------------------

var inspectProjectJSON bool

var inspectProjectCmd = &cobra.Command{
	Use:   "project",
	Short: "Inspect the current project (root, language, phelix.yaml validity)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectProjectJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		dir, err := currentDir()
		if err != nil {
			return err
		}
		view, err := buildProjectContext(dir)
		if err != nil {
			return err
		}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		printProjectSummary(view)
		return nil
	},
}

// --- inspect runtime -------------------------------------------------------------

var inspectRuntimeJSON bool

var inspectRuntimeCmd = &cobra.Command{
	Use:   "runtime",
	Short: "Inspect the runtime and toolchains Phelix depends on",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectRuntimeJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		view := buildRuntimeContext()
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		printRuntimeSummary(view)
		return nil
	},
}

// --- inspect config ---------------------------------------------------------------

var inspectConfigJSON bool

var inspectConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Inspect the effective phelix.yaml configuration (no secrets)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectConfigJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		dir, err := currentDir()
		if err != nil {
			return err
		}
		cfg, cfgErr := projectLoadForContext(dir)
		if cfgErr != nil && phelixerr.CodeOf(cfgErr) != phelixerr.CodeNotFound {
			// A malformed config is reported as a fact by the project section;
			// the config section needs a parseable file to describe.
			return cfgErr
		}
		view := buildConfigContext(cfg)
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		printConfigSummary(view)
		return nil
	},
}

// --- inspect app ---------------------------------------------------------------------

var inspectAppJSON bool

var inspectAppCmd = &cobra.Command{
	Use:   "app [ID|AppName]",
	Short: "Inspect one application's reconciled state",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectAppJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		identifier := ""
		if len(args) > 0 {
			identifier = args[0]
		}
		status, err := resolveInspectApp(identifier)
		if err != nil {
			return err
		}
		proxyUp, proxyByApp := proxySnapshot()
		view := buildAppContext(*status, proxyUp, proxyByApp)
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		printAppSummary(view)
		return nil
	},
}

// --- inspect deployment ----------------------------------------------------------------

var inspectDeploymentJSON bool

var inspectDeploymentCmd = &cobra.Command{
	Use:   "deployment [ID|AppName]",
	Short: "Inspect an application's deployment state (in-flight lock, strategy, canary)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectDeploymentJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		identifier := ""
		if len(args) > 0 {
			identifier = args[0]
		}
		status, err := resolveInspectApp(identifier)
		if err != nil {
			return err
		}
		state, loadErr := deploy.Load(status.Name)
		if loadErr != nil && !os.IsNotExist(loadErr) {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load deploy state", loadErr)
		}
		// No deploy.json is a fact (classic management / nothing deployed),
		// not a failure — buildDeploymentContext renders it as pending.
		view := buildDeploymentContext(state)
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		printDeploymentSummary(view)
		return nil
	},
}

// --- inspect versions ---------------------------------------------------------------------

var inspectVersionsJSON bool
var inspectVersionsLimit int

var inspectVersionsCmd = &cobra.Command{
	Use:   "versions [ID|AppName]",
	Short: "Inspect an application's recorded versions (bounded)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectVersionsJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		limit, err := validateContextLimit("limit", inspectVersionsLimit, contextDefaultVersions, contextMaxVersions)
		if err != nil {
			return err
		}
		identifier := ""
		if len(args) > 0 {
			identifier = args[0]
		}
		status, err := resolveInspectApp(identifier)
		if err != nil {
			return err
		}
		view, err := buildVersionsContext(status.Name, limit)
		if err != nil {
			return err
		}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		printVersionsSummary(view)
		return nil
	},
}

// --- inspect health ---------------------------------------------------------------------------

var inspectHealthJSON bool

var inspectHealthCmd = &cobra.Command{
	Use:   "health [ID|AppName]",
	Short: "Inspect an application's health checks (structured, redacted)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectHealthJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		identifier := ""
		if len(args) > 0 {
			identifier = args[0]
		}
		status, err := resolveInspectApp(identifier)
		if err != nil {
			return err
		}
		view, err := buildHealthContext(status.ID, status.Name)
		if err != nil {
			return err
		}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		printHealthSummary(view)
		return nil
	},
}

// --- inspect capabilities ------------------------------------------------------------------------

var inspectCapabilitiesJSON bool

var inspectCapabilitiesCmd = &cobra.Command{
	Use:   "capabilities",
	Short: "Discover what this Phelix build supports (capability registry)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectCapabilitiesJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		view := buildCapabilitiesContext()
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		fmt.Printf("Capabilities: %s\n", joinCapabilities(view.Capabilities))
		return nil
	},
}

// --- inspect operations -----------------------------------------------------------------------------

var inspectOperationsJSON bool
var inspectOperationsApp string
var inspectOperationsLimit int

var inspectOperationsCmd = &cobra.Command{
	Use:   "operations",
	Short: "Inspect recorded mutation operations (bounded, newest first)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if inspectOperationsJSON {
			restore := machine.EnterJSON()
			defer restore()
		}
		limit, err := validateContextLimit("limit", inspectOperationsLimit, contextDefaultOperations, contextMaxOperations)
		if err != nil {
			return err
		}
		appFilter := ""
		if inspectOperationsApp != "" {
			status, err := resolveInspectApp(inspectOperationsApp)
			if err != nil {
				return err
			}
			appFilter = status.Name
		}
		view, err := buildOperationsContext(appFilter, limit)
		if err != nil {
			return err
		}
		if machine.Active() {
			return writeEnvelopeResult(machine.Success("", view))
		}
		printOperationsSummary(view)
		return nil
	},
}

func init() {
	InspectCmd.AddCommand(inspectProjectCmd)
	InspectCmd.AddCommand(inspectRuntimeCmd)
	InspectCmd.AddCommand(inspectConfigCmd)
	InspectCmd.AddCommand(inspectAppCmd)
	InspectCmd.AddCommand(inspectDeploymentCmd)
	InspectCmd.AddCommand(inspectVersionsCmd)
	InspectCmd.AddCommand(inspectHealthCmd)
	InspectCmd.AddCommand(inspectCapabilitiesCmd)
	InspectCmd.AddCommand(inspectOperationsCmd)

	inspectProjectCmd.Flags().BoolVar(&inspectProjectJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectRuntimeCmd.Flags().BoolVar(&inspectRuntimeJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectConfigCmd.Flags().BoolVar(&inspectConfigJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectAppCmd.Flags().BoolVar(&inspectAppJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectDeploymentCmd.Flags().BoolVar(&inspectDeploymentJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectVersionsCmd.Flags().BoolVar(&inspectVersionsJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectVersionsCmd.Flags().IntVar(&inspectVersionsLimit, "limit", 0, "Maximum versions to return (0 = default 20, max 100)")
	inspectHealthCmd.Flags().BoolVar(&inspectHealthJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectCapabilitiesCmd.Flags().BoolVar(&inspectCapabilitiesJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectOperationsCmd.Flags().BoolVar(&inspectOperationsJSON, "json", false, "Output the machine envelope on stdout (summary moves to stderr)")
	inspectOperationsCmd.Flags().StringVar(&inspectOperationsApp, "app", "", "Filter operations by application")
	inspectOperationsCmd.Flags().IntVar(&inspectOperationsLimit, "limit", 0, "Maximum operations to return (0 = default 20, max 100)")
}

// --- human summaries (deterministic key: value lines; the machine contract
// lives in the --json envelopes) -----------------------------------------------------

func printProjectSummary(v *inspectProject) {
	fmt.Printf("Project:   %s\n", v.Root)
	if v.Language != "" {
		fmt.Printf("Language:  %s\n", v.Language)
	}
	if v.Name != "" {
		fmt.Printf("Name:      %s\n", v.Name)
	}
	if v.ConfigPath != "" {
		fmt.Printf("Config:    %s (found=%v valid=%v)\n", v.ConfigPath, v.ConfigFound, v.ConfigValid)
	}
	if v.ConfigError != "" {
		fmt.Printf("ConfigErr: %s\n", v.ConfigError)
	}
	if v.Port != 0 {
		fmt.Printf("Port:      %d\n", v.Port)
	}
}

func printRuntimeSummary(v *inspectRuntime) {
	fmt.Printf("OS/Arch:   %s/%s\n", v.OS, v.Architecture)
	// Ordered slice, not a map: human output is deterministic too.
	for _, entry := range []struct {
		name string
		t    *toolchainView
	}{{"go", v.Go}, {"rust", v.Rust}, {"docker", v.Docker}} {
		if entry.t == nil {
			continue
		}
		if entry.t.Installed {
			fmt.Printf("%-10s installed (%s)\n", entry.name+":", entry.t.Version)
		} else {
			fmt.Printf("%-10s not installed\n", entry.name+":")
		}
	}
}

func printConfigSummary(v *inspectConfig) {
	fmt.Printf("Source:    %s\n", v.Source)
	if v.Name != "" {
		fmt.Printf("Name:      %s\n", v.Name)
	}
	if v.Port != 0 {
		fmt.Printf("Port:      %d\n", v.Port)
	}
	if v.Deploy != nil {
		fmt.Printf("Deploy:    strategy=%s replicas=%d runtime=%s\n", v.Deploy.Strategy, v.Deploy.Replicas, v.Deploy.Runtime)
	}
	if v.Resources != nil {
		fmt.Printf("Resources: cpu=%s memory=%s\n", v.Resources.CPU, v.Resources.Memory)
	}
}

func printAppSummary(v *inspectApp) {
	fmt.Printf("App:       %s (%s)\n", v.Name, v.ID)
	fmt.Printf("Status:    %s (pid %d)\n", v.Status, v.PID)
	if v.Version != 0 {
		fmt.Printf("Version:   v%d %s\n", v.Version, v.Tag)
	}
	if v.DeployMode != "" {
		fmt.Printf("Deploy:    %s\n", v.DeployMode)
	}
}

func printDeploymentSummary(v *inspectDeployment) {
	fmt.Printf("Deployment: %s (strategy=%s)\n", v.Status, v.Strategy)
	if v.DeploymentID != "" {
		fmt.Printf("ID:         %s\n", v.DeploymentID)
	}
	if v.OpLock != nil {
		fmt.Printf("OpLock:     %s (pid %d)\n", v.OpLock.Operation, v.OpLock.PID)
	}
	if v.Canary != nil {
		fmt.Printf("Canary:     %s v%d step %d/%d\n", v.Canary.Status, v.Canary.Version, v.Canary.Step, v.Canary.Steps)
	}
}

func printVersionsSummary(v *inspectVersions) {
	if v.Current != nil {
		fmt.Printf("Current:   v%d %s\n", v.Current.Version, v.Current.Tag)
	}
	fmt.Printf("Available: %d version(s)", v.Count)
	if v.Truncated {
		fmt.Printf(" (truncated at limit %d)", v.Limit)
	}
	fmt.Println()
	for _, ver := range v.Available {
		fmt.Printf("  v%d %s\n", ver.Version, ver.Tag)
	}
}

func printHealthSummary(v *inspectHealth) {
	fmt.Printf("Health:    %s (source=%s)\n", v.Status, v.Source)
	if v.DeployTier != "" {
		fmt.Printf("Tier:      %s\n", v.DeployTier)
	}
	for _, c := range v.Checks {
		fmt.Printf("  %-16s %s\n", c.Name+":", c.Status)
	}
}

func printOperationsSummary(v *inspectOperationsView) {
	fmt.Printf("Operations: %d", v.Count)
	if v.Truncated {
		fmt.Printf(" (truncated at limit %d)", v.Limit)
	}
	fmt.Println()
	for _, op := range v.Operations {
		fmt.Printf("  %-22s %-9s %-10s %s\n", op.OperationID, op.Kind, op.Status, op.App)
	}
}
