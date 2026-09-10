package matrix

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/fatih/color"
)

// Report is the complete matrix build report, written as both a human-readable
// terminal summary and a machine-readable JSON file.
type Report struct {
	RunID       string    `json:"run_id,omitempty"` // Matrix Run this report belongs to
	AppName     string    `json:"app_name"`
	Lang        string    `json:"language"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Duration    string    `json:"total_duration"`
	Total       int       `json:"total"`
	Succeeded   int       `json:"succeeded"`
	Failed      int       `json:"failed"`
	Skipped     int       `json:"skipped"`
	// Pending counts combinations without a final outcome (pending or stale
	// running) — non-zero only for interrupted runs. Succeeded+Failed+Skipped+
	// Pending == Total always holds.
	Pending      int           `json:"pending,omitempty"`
	Combinations []ComboReport `json:"combinations"`
}

// ComboReport describes one matrix combination's outcome.
type ComboReport struct {
	Combination string `json:"combination"` // e.g. "go1.22-linux-amd64"
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Version     string `json:"toolchain_version"`
	Status      string `json:"status"` // "success", "failed", "skipped", "pending", "running"
	Duration    string `json:"duration"`
	Artifact    string `json:"artifact,omitempty"` // binary path or image tag
	// SHA256 is the full checksum of the final artifact's bytes (image digest
	// for Docker artifacts). The terminal summary shows a shortened form; the
	// complete value is always available in the JSON report.
	SHA256 string `json:"sha256,omitempty"`
	Error  string `json:"error,omitempty"`
	// CacheStatus is this combination's compiler-cache classification
	// ("cold"/"hit", empty when unknown). Part of the build-report
	// integration so every combination retains independent metrics.
	CacheStatus string `json:"cache_status,omitempty"`
	// Attempts is how many execution attempts the combination took (automatic
	// retries included); 1 when it succeeded (or failed) on the first try.
	Attempts int `json:"attempts,omitempty"`
	// AttemptLog records each attempt's outcome so a combination that
	// eventually succeeded after failures can be explained.
	AttemptLog []AttemptReport `json:"attempt_log,omitempty"`
}

// AttemptReport is one attempt's entry in a ComboReport.
type AttemptReport struct {
	Number   int    `json:"number"`
	Status   string `json:"status"`
	Duration string `json:"duration"`
	Error    string `json:"error,omitempty"`
}

// GenerateReport builds a Report from the execution results. An empty result
// set is valid (e.g. a dry-run plan) and never panics; error strings are
// redacted because report.json may contain compiler output fragments.
func GenerateReport(appName string, results []Result, startTime time.Time) *Report {
	r := &Report{
		AppName:      appName,
		StartedAt:    startTime,
		CompletedAt:  time.Now(),
		Total:        len(results),
		Combinations: make([]ComboReport, 0, len(results)),
	}
	if len(results) > 0 {
		r.Lang = string(results[0].Combination.Lang)
	}

	for _, res := range results {
		cr := ComboReport{
			Combination: res.Combination.ID(),
			OS:          res.Combination.OS,
			Arch:        res.Combination.Arch,
			Version:     res.Combination.Version,
			Status:      res.Status,
			Duration:    res.Duration.Round(time.Millisecond).String(),
			Artifact:    res.Artifact,
			SHA256:      res.SHA256,
			CacheStatus: res.CacheStatus,
			Attempts:    len(res.Attempts),
		}
		if cr.Attempts == 0 && res.Status != "" {
			cr.Attempts = 1
		}
		for _, a := range res.Attempts {
			ar := AttemptReport{
				Number:   a.Number,
				Status:   a.Status,
				Duration: a.Duration.Round(time.Millisecond).String(),
			}
			if a.Error != nil {
				ar.Error = phelixerr.Redact(a.Error.Error())
			}
			cr.AttemptLog = append(cr.AttemptLog, ar)
		}
		if res.Error != nil {
			cr.Error = phelixerr.Redact(res.Error.Error())
		}
		r.Combinations = append(r.Combinations, cr)

		switch res.Status {
		case "success":
			r.Succeeded++
		case "failed":
			r.Failed++
		case "skipped":
			r.Skipped++
		}
	}

	r.Duration = r.CompletedAt.Sub(r.StartedAt).Round(time.Second).String()
	return r
}

// ReportFromRun builds the Report from a Run's persisted combination records —
// the run-level view. A resumed run's report.json must describe the whole run
// (combinations that succeeded in earlier sessions included), not just the
// last session's results; building from the run record is what keeps
// report.json, the release manifest, and versions.json consistent with each
// other. Incomplete combinations (an interrupted session) appear with their
// run-record status and count toward Pending.
func ReportFromRun(run *Run) *Report {
	if run == nil {
		return &Report{Combinations: []ComboReport{}}
	}
	r := &Report{
		RunID:        string(run.ID),
		AppName:      run.AppName,
		Lang:         run.Config.Lang,
		StartedAt:    run.StartedAt,
		CompletedAt:  run.FinishedAt,
		Duration:     run.Duration,
		Total:        len(run.Combinations),
		Combinations: make([]ComboReport, 0, len(run.Combinations)),
	}
	if r.CompletedAt.IsZero() {
		r.CompletedAt = time.Now()
	}
	if r.Duration == "" {
		r.Duration = r.CompletedAt.Sub(r.StartedAt).Round(time.Second).String()
	}

	for _, rc := range run.Combinations {
		cr := ComboReport{
			Combination: rc.ID,
			OS:          rc.OS,
			Arch:        rc.Arch,
			Version:     rc.Version,
			Status:      rc.Status,
			Duration:    rc.Duration,
			Artifact:    rc.Artifact,
			SHA256:      rc.SHA256,
			Error:       rc.Error, // already redacted at record time
			CacheStatus: rc.CacheStatus,
			Attempts:    rc.Attempts,
		}
		if cr.Attempts == 0 && (rc.Status == "success" || rc.Status == "failed") {
			cr.Attempts = 1
		}
		for _, a := range rc.AttemptLog {
			cr.AttemptLog = append(cr.AttemptLog, AttemptReport{
				Number:   a.Number,
				Status:   a.Status,
				Duration: a.Duration,
				Error:    a.Error,
			})
		}
		r.Combinations = append(r.Combinations, cr)

		switch rc.Status {
		case "success":
			r.Succeeded++
		case "failed":
			r.Failed++
		case "skipped":
			r.Skipped++
		default: // pending, running (interrupted session)
			r.Pending++
		}
	}
	return r
}

// PrintTerminal writes a human-readable summary to stdout.
func (r *Report) PrintTerminal() {
	fmt.Println()

	// Header
	if r.RunID != "" {
		fmt.Printf("  Matrix Run: %s\n", color.CyanString(r.RunID))
	}
	switch {
	case r.Pending > 0:
		color.Yellow("  Matrix build interrupted — %d of %d combination(s) incomplete (resumable)\n", r.Pending, r.Total)
	case r.Failed > 0:
		color.Red("  Matrix build completed with %d failure(s)\n", r.Failed)
	case r.Skipped > 0:
		color.Yellow("  Matrix build completed (%d skipped, %d succeeded)\n", r.Skipped, r.Succeeded)
	default:
		color.Green("  Matrix build completed successfully\n")
	}

	fmt.Printf("  %s total: %d | succeeded: ", color.CyanString("Summary:"), r.Total)
	color.Green("%d", r.Succeeded)
	fmt.Printf(" | failed: ")
	color.Red("%d", r.Failed)
	fmt.Printf(" | skipped: %d", r.Skipped)
	if r.Pending > 0 {
		fmt.Printf(" | pending: %d", r.Pending)
	}
	fmt.Printf("\n  Duration: %s\n\n", r.Duration)

	// Per-combination results table.
	dim := color.New(color.Faint)
	for _, cr := range r.Combinations {
		var statusIcon string
		switch cr.Status {
		case "success":
			statusIcon = color.GreenString("✓")
		case "failed":
			statusIcon = color.RedString("✗")
		case "skipped":
			statusIcon = color.YellowString("○")
		case "running":
			statusIcon = color.CyanString("⟳")
		default: // pending
			statusIcon = color.BlueString("◌")
		}

		fmt.Printf("    %s %-35s %s", statusIcon, cr.Combination, dim.Sprint(cr.Duration))
		if cr.Attempts > 1 {
			fmt.Printf("  %s", dim.Sprintf("attempt %d", cr.Attempts))
		}
		if cr.Artifact != "" {
			fmt.Printf("  %s", dim.Sprint(cr.Artifact))
		}
		if cr.SHA256 != "" {
			// Compact form in the terminal; the full checksum lives in the
			// JSON report and in `matrix show`.
			fmt.Printf("\n      %s %s", dim.Sprint("SHA256:"), dim.Sprint(ShortSHA256(cr.SHA256)))
		}
		fmt.Println()

		if cr.Error != "" {
			fmt.Printf("      %s %s\n", color.RedString("error:"), cr.Error)
		}
	}

	fmt.Println()
}

// WriteJSON writes the report as a JSON file next to the build artifacts.
func (r *Report) WriteJSON(projectRoot string) (string, error) {
	reportDir := filepath.Join(projectRoot, "builds", "matrix")
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "create report dir")
	}

	path := filepath.Join(reportDir, "report.json")
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "marshal report")
	}

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "write report")
	}

	return path, nil
}
