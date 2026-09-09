package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// matrixStatusCmd implements `phelix matrix status`: the current/live state of
// a Matrix Run. Unlike `matrix list` (all recorded runs) and `matrix show`
// (full inspection of one run), status answers "what is happening right now":
// with no argument it selects the run currently being executed (persisted
// status "running" plus an execution lock held by a live process) and renders
// per-combination state with consistent aggregate counters. With a run ID it
// shows that run's state — running, orphaned, or final.
//
// The command reads the same persisted run records the execution engine
// writes incrementally (after every attempt start and every completed
// combination); there is no separate status state to drift.
var matrixStatusCmd = &cobra.Command{
	Use:   "status [run-id]",
	Short: "Show the current state of a Matrix Run (defaults to the active run)",
	Long: `Show the live state of a Matrix Run.

With no argument, selects the currently executing run (there is at most one
active run per machine in practice; when several execute concurrently the most
recently started one is shown — name a run explicitly to inspect another).

  phelix matrix status              # the active run, or "No active Matrix Run."
  phelix matrix status mx_20260909_8f31   # a specific run
  phelix matrix status --json       # machine-readable state

A run whose status is "running" but whose executing process is gone (crash,
SIGKILL, restart) is reported as orphaned — resumable, not executing. Completed
runs show their final state; a completed run never reports "running".`,
	Args:          cobra.MaximumNArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			id, err := matrix.ParseRunID(strings.TrimSpace(args[0]))
			if err != nil {
				return err
			}
			run, err := matrix.LoadRun(id)
			if err != nil {
				return err
			}
			return printMatrixStatus(run, matrixStatusJSON)
		}

		active, err := matrix.ActiveRuns()
		if err != nil {
			return err
		}
		if len(active) == 0 {
			fmt.Println("No active Matrix Run.")
			runs, _, lerr := matrix.ListRuns()
			if lerr == nil && len(runs) > 0 {
				latest := runs[0]
				fmt.Printf("  Most recent run: %s (%s) — inspect it with 'phelix matrix show %s'\n",
					color.CyanString(string(latest.ID)), latest.Status, latest.ID)
			}
			return nil
		}
		if len(active) > 1 {
			fmt.Printf("%s %d matrix runs are executing — showing the most recently started one; name a run for its own status.\n",
				color.YellowString("⚠"), len(active))
		}
		return printMatrixStatus(active[0], matrixStatusJSON)
	},
}

var matrixStatusJSON bool

func init() {
	matrixStatusCmd.Flags().BoolVar(&matrixStatusJSON, "json", false, "Output machine-readable JSON")
	MatrixCmd.AddCommand(matrixStatusCmd)
}

// matrixStatusJSONSnapshot is the --json view of one run's current state.
type matrixStatusJSONSnapshot struct {
	RunID        string                  `json:"run_id"`
	AppName      string                  `json:"app_name"`
	Status       string                  `json:"status"`
	Executing    bool                    `json:"executing"`
	PID          int                     `json:"pid,omitempty"`
	ParentRunID  string                  `json:"parent_run_id,omitempty"`
	ResumeCount  int                     `json:"resume_count,omitempty"`
	StartedAt    string                  `json:"started_at"`
	FinishedAt   string                  `json:"finished_at,omitempty"`
	Counters     matrix.StatusCounters   `json:"counters"`
	Release      *matrix.ReleaseManifest `json:"release,omitempty"`
	Combinations []matrixStatusCombo     `json:"combinations"`
}

// matrixStatusCombo is one combination inside the status snapshot.
type matrixStatusCombo struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts,omitempty"`
	// Attempt is the in-flight attempt number (running combinations only);
	// final attempt counts live in Attempts.
	Attempt   string `json:"attempt,omitempty"`
	Duration  string `json:"duration,omitempty"`
	Artifact  string `json:"artifact,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// printMatrixStatus renders one run's current state (terminal or JSON). It is
// read-only: state comes from the persisted run record, liveness from the
// run's execution lock, and the release view from the run's manifest.
func printMatrixStatus(run *matrix.Run, asJSON bool) error {
	counters := run.SnapshotCounters()
	executing, pid := matrix.RunLockOwner(run.ID)
	manifest, _ := matrix.LoadManifest(run.ID)

	if asJSON {
		snapshot := matrixStatusJSONSnapshot{
			RunID:       string(run.ID),
			AppName:     run.AppName,
			Status:      string(run.Status),
			Executing:   executing,
			PID:         pid,
			ParentRunID: string(run.ParentRunID),
			ResumeCount: run.ResumeCount,
			StartedAt:   run.StartedAt.Format(time.RFC3339),
			Counters:    counters,
			Release:     manifest,
		}
		if !run.FinishedAt.IsZero() {
			snapshot.FinishedAt = run.FinishedAt.Format(time.RFC3339)
		}
		for _, rc := range run.Combinations {
			combo := matrixStatusCombo{
				ID:        rc.ID,
				Status:    rc.Status,
				Attempts:  rc.Attempts,
				Duration:  rc.Duration,
				Artifact:  rc.Artifact,
				SizeBytes: fileSize(rc.Artifact),
				SHA256:    rc.SHA256,
			}
			if rc.Status == "running" {
				combo.Attempt = fmt.Sprintf("%d/%d", rc.Attempts, 1+run.Config.Retries)
				if rc.Attempts < 1 {
					combo.Attempt = ""
				}
			}
			snapshot.Combinations = append(snapshot.Combinations, combo)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(snapshot)
	}

	dim := color.New(color.Faint)
	fmt.Printf("Matrix Run %s — %s", color.CyanString(string(run.ID)), run.AppName)
	if run.Config.Lang != "" {
		fmt.Printf(" (%s)", run.Config.Lang)
	}
	fmt.Println()
	if run.ParentRunID != "" {
		fmt.Printf("Parent Run:  %s %s\n", run.ParentRunID, dim.Sprint("(manual retry run)"))
	}
	if run.ResumeCount > 0 {
		fmt.Printf("Resumed:     %d time(s)\n", run.ResumeCount)
	}

	statusLine := string(run.Status)
	switch {
	case executing:
		statusLine += dim.Sprintf(" — executing (PID %d)", pid)
	case run.Status == matrix.RunStatusRunning:
		statusLine += dim.Sprintf(" — orphaned (no executing process; resumable with 'phelix build --matrix --resume=%s')", run.ID)
	}
	fmt.Printf("Status:      %s\n", colorizeRunStatus(statusLine))
	fmt.Printf("Started:     %s\n", run.StartedAt.Format("2006-01-02 15:04:05"))
	if !run.FinishedAt.IsZero() {
		fmt.Printf("Finished:    %s\n", run.FinishedAt.Format("2006-01-02 15:04:05"))
	}
	if run.Duration != "" {
		fmt.Printf("Duration:    %s\n", run.Duration)
	}
	if manifest != nil {
		fmt.Printf("\nRelease:\n")
		version := fmt.Sprintf("v%d", manifest.Version)
		if manifest.Tag != "" {
			version += dim.Sprintf(" (tag %s)", manifest.Tag)
		}
		fmt.Printf("  Version:   %s\n", version)
		fmt.Printf("  Status:    %s\n", manifest.Status)
		fmt.Printf("  Artifacts: %d of %d combinations\n", len(manifest.Artifacts), manifest.TotalCombinations)
	}

	fmt.Printf("\n%d combination(s)\n", counters.Total)
	fmt.Println("──────────────────────────────────────────────")
	for _, rc := range run.Combinations {
		var icon, detail string
		switch rc.Status {
		case "success":
			icon = color.GreenString("✓")
			detail = rc.Duration
		case "failed":
			icon = color.RedString("✗")
			detail = rc.Duration
		case "running":
			icon = color.CyanString("⟳")
			switch {
			case executing && !rc.StartedAt.IsZero():
				detail = time.Since(rc.StartedAt).Round(time.Second).String()
			case executing:
				detail = "running"
			default:
				// The run is not executing: this entry was in flight when
				// the run stopped (interrupted or crashed) — its worker is
				// gone, and a resume will re-execute it.
				detail = "stale"
			}
			if rc.Attempts > 1 {
				detail += dim.Sprintf(" (attempt %d/%d)", rc.Attempts, 1+run.Config.Retries)
			}
		default: // pending
			icon = dim.Sprint("◌")
			detail = "pending"
		}
		fmt.Printf("  %s %-35s %s\n", icon, rc.ID, dim.Sprint(detail))
		if rc.Status == "success" {
			if rc.Artifact != "" {
				if size := fileSize(rc.Artifact); size > 0 {
					fmt.Printf("      %s %s\n", dim.Sprint("Binary:"), dim.Sprintf("%d B", size))
				}
			}
			if rc.SHA256 != "" {
				fmt.Printf("      %s %s\n", dim.Sprint("SHA256:"), dim.Sprint(matrix.ShortSHA256(rc.SHA256)))
			}
		}
	}

	fmt.Println("──────────────────────────────────────────────")
	fmt.Printf("Progress: %d/%d — success %d · failed %d · running %d · pending %d",
		counters.Complete(), counters.Total, counters.Succeeded, counters.Failed, counters.Running, counters.Pending)
	if counters.Skipped > 0 {
		fmt.Printf(" · skipped %d", counters.Skipped)
	}
	fmt.Println()
	if run.Status == matrix.RunStatusInterrupted || (run.Status == matrix.RunStatusRunning && !executing) {
		fmt.Printf("  %s resumable with 'phelix build --matrix --resume=%s'\n", dim.Sprint("→"), run.ID)
	}
	if run.Failed > 0 && !executing {
		fmt.Printf("  %s retry failures with 'phelix matrix retry %s --failed'\n", dim.Sprint("→"), run.ID)
	}
	fmt.Println()
	return nil
}
