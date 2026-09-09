package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
)

var (
	matrixListLimit int
	matrixListJSON  bool
	matrixShowJSON  bool
)

// MatrixCmd groups the matrix configuration and run-history commands.
var MatrixCmd = &cobra.Command{
	Use:   "matrix",
	Short: "Matrix build configuration and run history",
	Long: `Inspect Matrix build runs and configure the Build Matrix.

Subcommands:
  list             Show recorded Matrix Runs (newest first)
  show <run-id>    Show one Matrix Run: configuration snapshot and per-combination results
  retry <run-id>   Retry a Run's failed combinations in a new, linked Run
  init             Interactive wizard that writes a matrix profile into phelix.yaml`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var matrixListCmd = &cobra.Command{
	Use:           "list",
	Short:         "Show recorded Matrix Runs (read-only)",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		runs, skipped, err := matrix.ListRuns()
		if err != nil {
			return err
		}
		if skipped > 0 {
			fmt.Printf("%s %d malformed matrix run %s skipped\n",
				color.YellowString("⚠"), skipped, plural(skipped))
		}
		if matrixListLimit > 0 && len(runs) > matrixListLimit {
			runs = runs[:matrixListLimit]
		}

		if matrixListJSON {
			return printMatrixRunsJSON(runs)
		}

		fmt.Printf("Matrix Runs\n\n")
		if len(runs) == 0 {
			fmt.Println("No matrix runs recorded yet — build one with 'phelix build --matrix' or a phelix.yaml matrix profile.")
			return nil
		}

		table := tablewriter.NewTable(os.Stdout)
		table.Header([]string{"ID", "STATUS", "APP", "COMBINATIONS", "STARTED"})
		for _, r := range runs {
			table.Append([]string{
				string(r.ID),
				colorizeRunStatus(string(r.Status)),
				r.AppName,
				fmt.Sprintf("%d/%d", r.Succeeded, r.Total),
				r.StartedAt.Format("2006-01-02 15:04:05"),
			})
		}
		table.Render()
		return nil
	},
}

var matrixShowCmd = &cobra.Command{
	Use:           "show <run-id>",
	Short:         "Show one Matrix Run (configuration snapshot and per-combination results)",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := matrix.ParseRunID(strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}
		run, err := matrix.LoadRun(id)
		if err != nil {
			return err
		}

		if matrixShowJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(run)
		}
		printMatrixRun(run)
		return nil
	},
}

func init() {
	matrixListCmd.Flags().IntVar(&matrixListLimit, "limit", 20, "Maximum number of runs to show (0 = all)")
	matrixListCmd.Flags().BoolVar(&matrixListJSON, "json", false, "Output machine-readable JSON")
	matrixShowCmd.Flags().BoolVar(&matrixShowJSON, "json", false, "Output machine-readable JSON")

	MatrixCmd.AddCommand(matrixListCmd)
	MatrixCmd.AddCommand(matrixShowCmd)
	MatrixCmd.AddCommand(matrixInitCmd)
}

// printMatrixRunsJSON emits the run summaries as a stable JSON array (empty
// array, never null, when there is no history).
func printMatrixRunsJSON(runs []*matrix.Run) error {
	type summary struct {
		ID          matrix.RunID `json:"id"`
		AppName     string       `json:"app_name"`
		Status      string       `json:"status"`
		Total       int          `json:"total"`
		Succeeded   int          `json:"succeeded"`
		Failed      int          `json:"failed"`
		Skipped     int          `json:"skipped"`
		Incomplete  int          `json:"incomplete,omitempty"`
		ParentRunID matrix.RunID `json:"parent_run_id,omitempty"`
		StartedAt   string       `json:"started_at"`
		FinishedAt  string       `json:"finished_at,omitempty"`
		Duration    string       `json:"duration,omitempty"`
	}
	out := make([]summary, 0, len(runs))
	for _, r := range runs {
		s := summary{
			ID: r.ID, AppName: r.AppName, Status: string(r.Status),
			Total: r.Total, Succeeded: r.Succeeded, Failed: r.Failed, Skipped: r.Skipped,
			Incomplete:  r.Incomplete,
			ParentRunID: r.ParentRunID,
			StartedAt:   r.StartedAt.Format("2006-01-02T15:04:05Z07:00"),
			Duration:    r.Duration,
		}
		if !r.FinishedAt.IsZero() {
			s.FinishedAt = r.FinishedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, s)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// printMatrixRun renders one run: identity, timing, the configuration
// snapshot taken at execution time, per-combination results (with attempt
// history), and the summary. Failed combinations show their (redacted,
// bounded) error — never full logs.
func printMatrixRun(run *matrix.Run) {
	dim := color.New(color.Faint)

	fmt.Printf("Matrix Run: %s\n\n", color.CyanString(string(run.ID)))
	fmt.Printf("Status:      %s\n", colorizeRunStatus(string(run.Status)))
	fmt.Printf("App:         %s\n", run.AppName)
	if run.ParentRunID != "" {
		fmt.Printf("Parent Run:  %s %s\n", run.ParentRunID, dim.Sprint("(manual retry of this run's source)"))
	}
	if run.ResumeCount > 0 {
		fmt.Printf("Resumed:     %d time(s)\n", run.ResumeCount)
	}
	fmt.Printf("Started:     %s\n", run.StartedAt.Format("2006-01-02 15:04:05"))
	if !run.FinishedAt.IsZero() {
		fmt.Printf("Finished:    %s\n", run.FinishedAt.Format("2006-01-02 15:04:05"))
	}
	if run.Duration != "" {
		fmt.Printf("Duration:    %s\n", run.Duration)
	}

	fmt.Printf("\nConfiguration (snapshot at execution time):\n")
	fmt.Printf("  Language:   %s %s\n", run.Config.Lang, dim.Sprintf("(%s)", run.Config.Sources.Lang))
	fmt.Printf("  Versions:   %s %s\n", strings.Join(run.Config.Versions, ", "), dim.Sprintf("(%s)", run.Config.Sources.Versions))
	fmt.Printf("  Platforms:  %s %s\n", strings.Join(run.Config.Platforms, ", "), dim.Sprintf("(%s)", run.Config.Sources.Platforms))
	fmt.Printf("  Concurrency: %d %s\n", run.Config.Concurrency, dim.Sprintf("(%s)", run.Config.Sources.Concurrency))
	if run.Config.Retries > 0 {
		fmt.Printf("  Retries:    %d %s\n", run.Config.Retries, dim.Sprintf("(%s)", run.Config.Sources.Retries))
	}
	for _, rule := range run.Config.Include {
		fmt.Printf("  Include:    %s\n", rule.Describe())
	}
	for _, rule := range run.Config.Exclude {
		fmt.Printf("  Exclude:    %s\n", rule.Describe())
	}

	fmt.Printf("\nCombinations:\n\n")
	for _, c := range run.Combinations {
		var icon string
		switch c.Status {
		case "success":
			icon = color.GreenString("✓")
		case "failed":
			icon = color.RedString("✗")
		case "pending", "running", "":
			icon = color.BlueString("◌")
		default:
			icon = color.YellowString("○")
		}
		attempts := ""
		if c.Attempts > 1 {
			attempts = dim.Sprintf(" (attempt %d)", c.Attempts)
		}
		fmt.Printf("  %s %-35s %s%s\n", icon, c.ID, dim.Sprint(c.Duration), attempts)
		for _, a := range c.AttemptLog {
			marker := color.RedString("✗")
			if a.Status != "failed" {
				marker = color.GreenString("✓")
			}
			fmt.Printf("      %s attempt %d: %s %s\n", marker, a.Number, a.Status, dim.Sprint(a.Duration))
			if a.Error != "" {
				fmt.Printf("        %s %s\n", color.RedString("error:"), a.Error)
			}
		}
		if c.Error != "" && len(c.AttemptLog) == 0 {
			fmt.Printf("      %s %s\n", color.RedString("error:"), c.Error)
		}
	}

	fmt.Printf("\nSummary:\n")
	fmt.Printf("  Total:    %d\n", run.Total)
	fmt.Printf("  Success:  %d\n", run.Succeeded)
	fmt.Printf("  Failed:   %d\n", run.Failed)
	if run.Skipped > 0 {
		fmt.Printf("  Skipped:  %d\n", run.Skipped)
	}
	if run.Incomplete > 0 {
		fmt.Printf("  Incomplete: %d %s\n", run.Incomplete, dim.Sprint("(resumable with 'phelix build --matrix --resume')"))
	}
	fmt.Println()
}

func colorizeRunStatus(status string) string {
	switch status {
	case string(matrix.RunStatusSucceeded):
		return color.GreenString(status)
	case string(matrix.RunStatusPartial), string(matrix.RunStatusInterrupted):
		return color.YellowString(status)
	case string(matrix.RunStatusFailed):
		return color.RedString(status)
	default:
		return color.BlueString(status)
	}
}

// matrixResolveInput assembles the convergence input from the CLI flags of
// the invoking command and the loaded phelix.yaml. detectedLang only matters
// when no explicit version list names the ecosystem; passing any value is
// safe for callers that only need the activity decision.
func matrixResolveInput(cmd *cobra.Command, projCfg *project.Config, detectedLang builder.Language) matrix.ResolveInput {
	in := matrix.ResolveInput{
		DetectedLang:  detectedLang,
		MatrixFlag:    matrixFlag,
		MatrixFlagSet: cmd.Flags().Changed("matrix"),
		CLI: matrix.CLIOptions{
			GoVersions:   goVersions,
			RustVersions: rustVersions,
			Platforms:    platforms,
		},
	}
	if cmd.Flags().Changed("matrix-concurrency") {
		in.CLI.Concurrency = matrixConcurrency
	}
	// Lookup (not Changed) so commands without the flag — e.g. test scratch
	// commands — do not panic on the access.
	if f := cmd.Flags().Lookup("matrix-retries"); f != nil && f.Changed {
		in.CLI.Retries = matrixRetries
	}
	if projCfg != nil && projCfg.Matrix != nil {
		in.YAML = projCfg.Matrix.MatrixProfile()
		in.YAMLEnabled = projCfg.Matrix.Enabled
	}
	return in
}

// matrixActive reports whether this invocation runs a matrix build, using the
// same rule the profile convergence applies (CLI flags and the phelix.yaml
// matrix profile included).
func matrixActive(cmd *cobra.Command, projCfg *project.Config) bool {
	return matrix.IsActive(matrixResolveInput(cmd, projCfg, builder.Go))
}

// resolveMatrixProfile converges CLI flags and the phelix.yaml matrix profile
// into the single normalized profile the matrix engine executes. The profile
// is validated (and expanded once) here so a misconfiguration fails with a
// crisp message before any build output starts.
func resolveMatrixProfile(cmd *cobra.Command, projCfg *project.Config, lang builder.Language) (*matrix.Profile, error) {
	prof, active, err := matrix.Resolve(matrixResolveInput(cmd, projCfg, lang))
	if err != nil {
		return nil, err
	}
	if !active || prof == nil {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "matrix mode is not active")
	}
	if _, err := prof.Plan(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid matrix build plan", err)
	}
	return prof, nil
}
