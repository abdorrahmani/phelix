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
	AppName      string        `json:"app_name"`
	Lang         string        `json:"language"`
	StartedAt    time.Time     `json:"started_at"`
	CompletedAt  time.Time     `json:"completed_at"`
	Duration     string        `json:"total_duration"`
	Total        int           `json:"total"`
	Succeeded    int           `json:"succeeded"`
	Failed       int           `json:"failed"`
	Skipped      int           `json:"skipped"`
	Combinations []ComboReport `json:"combinations"`
}

// ComboReport describes one matrix combination's outcome.
type ComboReport struct {
	Combination string `json:"combination"` // e.g. "go1.22-linux-amd64"
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Version     string `json:"toolchain_version"`
	Status      string `json:"status"` // "success", "failed", "skipped"
	Duration    string `json:"duration"`
	Artifact    string `json:"artifact,omitempty"` // binary path or image tag
	Error       string `json:"error,omitempty"`
	// CacheStatus is this combination's compiler-cache classification
	// ("cold"/"hit", empty when unknown). Part of the build-report
	// integration so every combination retains independent metrics.
	CacheStatus string `json:"cache_status,omitempty"`
}

// GenerateReport builds a Report from the execution results.
func GenerateReport(appName string, results []Result, startTime time.Time) *Report {
	r := &Report{
		AppName:      appName,
		Lang:         string(results[0].Combination.Lang),
		StartedAt:    startTime,
		CompletedAt:  time.Now(),
		Total:        len(results),
		Combinations: make([]ComboReport, 0, len(results)),
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
			CacheStatus: res.CacheStatus,
		}
		if res.Error != nil {
			cr.Error = res.Error.Error()
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

// PrintTerminal writes a human-readable summary to stdout.
func (r *Report) PrintTerminal() {
	fmt.Println()

	// Header
	if r.Failed > 0 {
		color.Red("  Matrix build completed with %d failure(s)\n", r.Failed)
	} else if r.Skipped > 0 {
		color.Yellow("  Matrix build completed (%d skipped, %d succeeded)\n", r.Skipped, r.Succeeded)
	} else {
		color.Green("  Matrix build completed successfully\n")
	}

	fmt.Printf("  %s total: %d | succeeded: ", color.CyanString("Summary:"), r.Total)
	color.Green("%d", r.Succeeded)
	fmt.Printf(" | failed: ")
	color.Red("%d", r.Failed)
	fmt.Printf(" | skipped: %d\n", r.Skipped)
	fmt.Printf("  Duration: %s\n\n", r.Duration)

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
		}

		fmt.Printf("    %s %-35s %s", statusIcon, cr.Combination, dim.Sprint(cr.Duration))
		if cr.Artifact != "" {
			fmt.Printf("  %s", dim.Sprint(cr.Artifact))
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
