package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
  status [run-id]  Show the current state of a Matrix Run (defaults to the active run)
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
		// The malformed-file warning is terminal output only: in --json mode
		// stdout must stay pure, machine-readable JSON.
		if skipped > 0 && !matrixListJSON {
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
		// Include-rule metadata (e.g. tag: latest) travels with the
		// combination into the run record; show renders it so the attributes
		// are visible where the README promises them.
		if len(c.Metadata) > 0 {
			keys := make([]string, 0, len(c.Metadata))
			for k := range c.Metadata {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+"="+c.Metadata[k])
			}
			fmt.Printf("      %s %s\n", dim.Sprint("Metadata:"), dim.Sprint(strings.Join(parts, " ")))
		}
		if c.Artifact != "" {
			fmt.Printf("      %s %s\n", dim.Sprint("Artifact:"), dim.Sprint(c.Artifact))
		}
		if c.SHA256 != "" {
			fmt.Printf("      %s %s\n", dim.Sprint("SHA256:"), dim.Sprint(matrix.ShortSHA256(c.SHA256)))
		}
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

	if manifest, err := matrix.LoadManifest(run.ID); err == nil && manifest != nil {
		fmt.Printf("\nRelease:\n")
		version := fmt.Sprintf("v%d", manifest.Version)
		if manifest.Tag != "" {
			version += dim.Sprintf(" (tag %s)", manifest.Tag)
		}
		fmt.Printf("  Version:   %s\n", version)
		fmt.Printf("  Status:    %s", manifest.Status)
		if manifest.Status == matrix.ReleaseStatusPartial {
			fmt.Print(dim.Sprint(" — some combinations failed; the manifest lists only successful artifacts"))
		}
		fmt.Println()
		fmt.Printf("  Artifacts: %d of %d combinations\n", len(manifest.Artifacts), manifest.TotalCombinations)
		if path, perr := matrix.ManifestPath(run.ID); perr == nil {
			fmt.Printf("  Manifest:  %s\n", dim.Sprint(path))
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

// matrixFlagConflict reports the contradictory combination of an explicit
// --matrix=false with matrix dimension flags. Silently ignoring explicitly
// requested versions/platforms (the old behavior, which ran a plain single
// build) hides user error behind surprising output, so it is rejected up
// front. matrix.Resolve applies the same rule at the engine level; this cmd
// guard makes the branch decision itself fail fast.
func matrixFlagConflict(matrixFlagSet, matrixFlag bool, goVers, rustVers, platforms []string) error {
	if !matrixFlagSet || matrixFlag {
		return nil
	}
	if len(goVers) > 0 || len(rustVers) > 0 || len(platforms) > 0 {
		return phelixerr.New(phelixerr.CodeInvalidArgument,
			"--matrix=false cannot be combined with --go-versions/--rust-versions/--platforms — omit the dimension flags or drop --matrix=false")
	}
	return nil
}

// validateMatrixFlagValues rejects explicitly-invalid numeric matrix flags
// instead of silently falling back to the defaults: --matrix-concurrency must
// be at least 1 and --matrix-retries at least 0. The *Set parameters tell
// whether the flag was explicitly provided (an unset flag keeps its default,
// which is valid).
func validateMatrixFlagValues(concurrency int, concurrencySet bool, retries int, retriesSet bool) error {
	if concurrencySet && concurrency <= 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"--matrix-concurrency must be at least 1, got %d", concurrency)
	}
	if retriesSet && retries < 0 {
		return phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"--matrix-retries must be 0 or greater, got %d", retries)
	}
	return nil
}

// dockerizeMatrixResolveInput assembles the convergence input for the
// dockerize command from its own matrix flags and the loaded phelix.yaml —
// the exact analogue of matrixResolveInput for `phelix build`, so both
// commands activate, validate, and expand the matrix identically.
func dockerizeMatrixResolveInput(cmd *cobra.Command, projCfg *project.Config, detectedLang builder.Language) matrix.ResolveInput {
	in := matrix.ResolveInput{
		DetectedLang:  detectedLang,
		MatrixFlag:    dockerizeMatrix,
		MatrixFlagSet: cmd.Flags().Changed("matrix"),
		CLI: matrix.CLIOptions{
			GoVersions:   dockerizeGoVersions,
			RustVersions: dockerizeRustVersions,
			Platforms:    dockerizePlatforms,
		},
	}
	if cmd.Flags().Changed("matrix-concurrency") {
		in.CLI.Concurrency = dockerizeConcurrency
	}
	if f := cmd.Flags().Lookup("matrix-retries"); f != nil && f.Changed {
		in.CLI.Retries = dockerizeMatrixRetries
	}
	if projCfg != nil && projCfg.Matrix != nil {
		in.YAML = projCfg.Matrix.MatrixProfile()
		in.YAMLEnabled = projCfg.Matrix.Enabled
	}
	return in
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
