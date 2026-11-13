package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/hooks"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/table"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	hooksJSON       bool
	hooksStatus     string
	hooksName       string
	hooksDeployment string
	hooksLimit      int
	hooksLast       bool
)

var HooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Inspect deployment lifecycle hook configuration and execution history",
}

var hooksListCmd = &cobra.Command{
	Use:           "list [ID|AppName]",
	Short:         "List the lifecycle hooks configured for an application",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runHooksList,
}

var hooksStatusCmd = &cobra.Command{
	Use:           "status [ID|AppName]",
	Short:         "Show the latest lifecycle hook status for an application",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runHooksStatus,
}

var hooksHistoryCmd = &cobra.Command{
	Use:           "history [ID|AppName]",
	Short:         "Show lifecycle hook execution history",
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runHooksHistory,
}

var hooksInspectCmd = &cobra.Command{
	Use:           "inspect <execution-id> [ID|AppName]",
	Short:         "Show one lifecycle hook execution with its captured output",
	Args:          cobra.RangeArgs(1, 2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runHooksInspect,
}

func init() {
	hooksListCmd.Flags().BoolVar(&hooksJSON, "json", false, "Output machine-readable JSON")
	hooksStatusCmd.Flags().BoolVar(&hooksJSON, "json", false, "Output machine-readable JSON")
	hooksHistoryCmd.Flags().BoolVar(&hooksJSON, "json", false, "Output machine-readable JSON")
	hooksHistoryCmd.Flags().StringVar(&hooksName, "hook", "", "Filter by hook name (pre_build, post_deploy, ...)")
	hooksHistoryCmd.Flags().StringVar(&hooksStatus, "status", "", "Filter by execution status (succeeded, failed, timed_out, cancelled, uncertain, skipped)")
	hooksHistoryCmd.Flags().StringVar(&hooksDeployment, "deployment", "", "Filter by deployment id")
	hooksHistoryCmd.Flags().IntVar(&hooksLimit, "limit", 20, "Maximum number of executions to show")
	hooksInspectCmd.Flags().BoolVar(&hooksJSON, "json", false, "Output machine-readable JSON")
	hooksInspectCmd.Flags().BoolVar(&hooksLast, "last", false, "Inspect the most recent execution instead of an id")

	HooksCmd.AddCommand(hooksListCmd, hooksStatusCmd, hooksHistoryCmd, hooksInspectCmd)
}

func resolveHooksApp(args []string) (*app.AppInfo, error) {
	identifier := ""
	if len(args) > 0 {
		identifier = args[0]
	} else if cfg, err := loadProjectConfig(); err == nil && cfg != nil && cfg.Name != "" {
		identifier = cfg.Name
	}
	if identifier == "" {
		if !IsInteractive() {
			return nil, phelixerr.New(phelixerr.CodeInvalidArgument,
				"missing required <ID|AppName>; usage: phelix hooks <command> <ID|AppName>")
		}
		chosen, err := PromptApp(false, "Select application")
		if err != nil {
			return nil, err
		}
		identifier = chosen
	}
	if err := app.Manager.LoadState(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load app state", err)
	}
	return GetAppInfo(identifier)
}

func hooksForApp(info *app.AppInfo) (*project.Config, *hooks.Store, error) {
	cfg, err := hookConfigFromDir(info.Directory)
	if err != nil {
		return nil, nil, err
	}
	return cfg, hooks.NewStore(info.Name), nil
}

func runHooksList(cmd *cobra.Command, args []string) error {
	info, err := resolveHooksApp(args)
	if err != nil {
		return err
	}
	cfg, _, err := hooksForApp(info)
	if err != nil {
		return err
	}
	var configured []string
	if cfg != nil && cfg.Hooks != nil {
		configured = cfg.Hooks.Configured()
	}
	if hooksJSON {
		type item struct {
			Name            string   `json:"name"`
			Enabled         bool     `json:"enabled"`
			Command         []string `json:"command"`
			Timeout         string   `json:"timeout"`
			Retries         int      `json:"retries"`
			ContinueOnError bool     `json:"continue_on_error"`
			WorkingDir      string   `json:"working_dir,omitempty"`
		}
		out := make([]item, 0, len(configured))
		for _, name := range configured {
			hook := cfg.Hooks.Get(name)
			out = append(out, item{
				Name: name, Enabled: hook.IsEnabled(), Command: hook.Command,
				Timeout: hook.TimeoutDuration().String(), Retries: hook.Retries,
				ContinueOnError: hook.ContinuesOnError(name), WorkingDir: hook.WorkingDir,
			})
		}
		return writeHooksJSON(map[string]any{"app": info.Name, "hooks": out})
	}
	if len(configured) == 0 {
		fmt.Printf("No lifecycle hooks configured for %s.\n", color.CyanString("'%s'", info.Name))
		return nil
	}
	t := table.New(os.Stdout, false)
	t.Header([]string{"Hook", "State", "Timeout", "Attempts", "Continue on error", "Command"})
	for _, name := range configured {
		hook := cfg.Hooks.Get(name)
		state := "enabled"
		if !hook.IsEnabled() {
			state = "disabled"
		}
		t.Append([]string{
			name, state, hook.TimeoutDuration().String(),
			strconv.Itoa(1 + hook.Retries),
			strconv.FormatBool(hook.ContinuesOnError(name)),
			strings.Join(hook.Command, " "),
		})
	}
	return t.Render()
}

func runHooksStatus(cmd *cobra.Command, args []string) error {
	info, err := resolveHooksApp(args)
	if err != nil {
		return err
	}
	_, store, err := hooksForApp(info)
	if err != nil {
		return err
	}
	executions, err := store.List()
	if err != nil {
		return err
	}
	latest := make(map[string]*hooks.Execution)
	counts := make(map[string]int)
	for _, exec := range executions {
		counts[exec.Status]++
		if prior, ok := latest[exec.Hook]; !ok || exec.StartedAt.After(prior.StartedAt) {
			latest[exec.Hook] = exec
		}
	}
	if hooksJSON {
		return writeHooksJSON(map[string]any{
			"app": info.Name, "counts": counts, "latest": latest,
		})
	}
	fmt.Printf("Lifecycle hook status for %s\n", color.CyanString("'%s'", info.Name))
	if len(executions) == 0 {
		fmt.Println("No hook executions recorded.")
		return nil
	}
	t := table.New(os.Stdout, false)
	t.Header([]string{"Hook", "Status", "Attempt", "Started", "Duration", "Exit"})
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		exec := latest[name]
		exit := "-"
		if exec.ExitCode != nil {
			exit = strconv.Itoa(*exec.ExitCode)
		}
		t.Append([]string{name, exec.Status, strconv.Itoa(exec.Attempt),
			exec.StartedAt.Local().Format("2006-01-02 15:04:05"),
			(time.Duration(exec.DurationMS) * time.Millisecond).String(), exit})
	}
	if err := t.Render(); err != nil {
		return err
	}
	fmt.Printf("\nSucceeded: %d  Failed: %d  Running: %d  Uncertain: %d\n",
		counts[hooks.StatusSucceeded], counts[hooks.StatusFailed]+counts[hooks.StatusTimedOut],
		counts[hooks.StatusRunning], counts[hooks.StatusUncertain])
	return nil
}

func runHooksHistory(cmd *cobra.Command, args []string) error {
	info, err := resolveHooksApp(args)
	if err != nil {
		return err
	}
	_, store, err := hooksForApp(info)
	if err != nil {
		return err
	}
	executions, err := store.List()
	if err != nil {
		return err
	}
	filtered := make([]*hooks.Execution, 0, len(executions))
	for _, exec := range executions {
		if hooksName != "" && exec.Hook != hooksName {
			continue
		}
		if hooksStatus != "" && exec.Status != hooksStatus {
			continue
		}
		if hooksDeployment != "" && exec.DeploymentID != hooksDeployment {
			continue
		}
		filtered = append(filtered, exec)
	}
	limit := hooksLimit
	if limit <= 0 || limit > len(filtered) {
		limit = len(filtered)
	}
	filtered = filtered[:limit]
	if hooksJSON {
		return writeHooksJSON(map[string]any{"app": info.Name, "executions": filtered})
	}
	if len(filtered) == 0 {
		fmt.Printf("No hook executions match the filters for %s.\n", color.CyanString("'%s'", info.Name))
		return nil
	}
	t := table.New(os.Stdout, false)
	t.Header([]string{"ID", "Hook", "Status", "Attempt", "Deployment", "Started", "Duration", "Exit"})
	for _, exec := range filtered {
		exit := "-"
		if exec.ExitCode != nil {
			exit = strconv.Itoa(*exec.ExitCode)
		}
		t.Append([]string{exec.ID, exec.Hook, exec.Status, strconv.Itoa(exec.Attempt),
			exec.DeploymentID, exec.StartedAt.Local().Format("2006-01-02 15:04:05"),
			(time.Duration(exec.DurationMS) * time.Millisecond).String(), exit})
	}
	return t.Render()
}

func runHooksInspect(cmd *cobra.Command, args []string) error {
	identifier := ""
	if len(args) > 1 {
		identifier = args[1]
	}
	info, err := resolveHooksApp([]string{identifier})
	if err != nil {
		return err
	}
	_, store, err := hooksForApp(info)
	if err != nil {
		return err
	}
	var exec *hooks.Execution
	if hooksLast {
		executions, lerr := store.List()
		if lerr != nil {
			return lerr
		}
		if len(executions) == 0 {
			return phelixerr.Newf(phelixerr.CodeNotFound, "no hook executions recorded for %q", info.Name)
		}
		exec = executions[0]
	} else {
		exec, err = store.Load(args[0])
		if err != nil {
			return err
		}
	}
	if hooksJSON {
		return writeHooksJSON(exec)
	}
	fmt.Printf("Execution %s\n", color.CyanString(exec.ID))
	fmt.Printf("  Application: %s\n", exec.AppName)
	fmt.Printf("  Hook:        %s\n", exec.Hook)
	fmt.Printf("  Status:      %s\n", exec.Status)
	fmt.Printf("  Deployment:  %s\n", exec.DeploymentID)
	fmt.Printf("  Strategy:    %s\n", exec.Strategy)
	fmt.Printf("  Version:     %d (previous %d)\n", exec.Version, exec.PreviousVersion)
	fmt.Printf("  Attempt:     %d\n", exec.Attempt)
	fmt.Printf("  Command:     %s\n", strings.Join(exec.Command, " "))
	fmt.Printf("  Directory:   %s\n", exec.WorkingDir)
	fmt.Printf("  Timeout:     %s\n", (time.Duration(exec.TimeoutMS) * time.Millisecond).String())
	fmt.Printf("  Started:     %s\n", exec.StartedAt.Local().Format(time.RFC3339))
	if !exec.FinishedAt.IsZero() {
		fmt.Printf("  Finished:    %s\n", exec.FinishedAt.Local().Format(time.RFC3339))
	}
	fmt.Printf("  Duration:    %s\n", (time.Duration(exec.DurationMS) * time.Millisecond).String())
	if exec.ExitCode != nil {
		fmt.Printf("  Exit code:   %d\n", *exec.ExitCode)
	}
	if exec.FailureReason != "" {
		fmt.Printf("  Failure:     %s\n", exec.FailureReason)
	}
	if exec.Stdout != "" {
		fmt.Printf("\nstdout%s:\n%s\n", truncatedLabel(exec.StdoutTruncated), exec.Stdout)
	}
	if exec.Stderr != "" {
		fmt.Printf("\nstderr%s:\n%s\n", truncatedLabel(exec.StderrTruncated), exec.Stderr)
	}
	return nil
}

func truncatedLabel(truncated bool) string {
	if truncated {
		return " (truncated)"
	}
	return ""
}

func writeHooksJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
