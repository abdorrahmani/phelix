package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/hostsecurity"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/securityscore"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/spf13/cobra"
)

func init() { SecurityCmd.AddCommand(newServerSecurityCommand()) }

// newServerIntelClient returns the HTTP client used to fetch host advisory
// sources. The nil default lets the intelligence layer apply its own
// conservative transport; tests replace it to redirect advisory traffic to a
// local server.
var newServerIntelClient = func() *http.Client { return nil }

// newServerHostSystem returns the system inspector for server security
// scans. Tests replace it to scan a fixture filesystem root.
var newServerHostSystem = func() hostsecurity.System { return hostsecurity.LocalSystem() }

// serverIntelligence builds the advisory intelligence used by server scans,
// caching fetched sources under the data directory so repeated scans reuse
// provenance-tracked snapshots.
func serverIntelligence(dataDir string) *hostsecurity.Intelligence {
	return &hostsecurity.Intelligence{
		CacheDir: filepath.Join(dataDir, "host-security", "intel-cache"),
		HTTP:     newServerIntelClient(),
	}
}

func newServerSecurityCommand() *cobra.Command {
	var jsonOutput bool
	root := &cobra.Command{Use: "server", Short: "Inspect this server's operating system packages and security", Args: cobra.NoArgs}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "emit a structured result")
	root.PersistentFlags().Duration("timeout", 5*time.Minute, "maximum operation duration")
	for _, name := range []string{"scan", "status", "report", "inventory", "history"} {
		name := name
		sub := &cobra.Command{Use: name, Short: map[string]string{"scan": "Collect a read-only host package snapshot and evaluate advisories", "status": "Show the latest host scan and scanner availability", "report": "List findings of the latest or a chosen host scan", "inventory": "List installed packages from a recorded host snapshot", "history": "List immutable host scan records"}[name], Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true}
		sub.Flags().Int("limit", 100, "maximum results (1..500)")
		sub.Flags().Int("offset", 0, "results to skip")
		sub.Flags().String("scan-id", "", "inspect a specific historical scan")
		sub.Flags().String("package", "", "filter package names")
		if name == "scan" {
			sub.Flags().Bool("full", false, "collect a full inventory (the default)")
		}
		if name == "report" {
			sub.Flags().String("severity", "", "filter findings by severity, e.g. high")
			sub.Flags().String("state", "", "filter findings by match state, e.g. confirmed")
		}
		sub.RunE = func(cmd *cobra.Command, args []string) error {
			if jsonOutput {
				defer machine.EnterJSON()()
			}
			limit, _ := cmd.Flags().GetInt("limit")
			offset, _ := cmd.Flags().GetInt("offset")
			if err := hostsecurity.ValidatePage(limit, offset); err != nil {
				return phelixerr.New(phelixerr.CodeInvalidArgument, err.Error())
			}
			duration, _ := cmd.Flags().GetDuration("timeout")
			if duration <= 0 || duration > 30*time.Minute {
				return phelixerr.New(phelixerr.CodeInvalidArgument, "timeout must be positive and at most 30m")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), duration)
			defer cancel()
			store, err := hostsecurity.OpenStore(server.DataDir())
			if err != nil {
				return err
			}
			defer store.Close()
			id, err := server.EnsureAgentID()
			if err != nil {
				return err
			}
			scanID, _ := cmd.Flags().GetString("scan-id")
			search, _ := cmd.Flags().GetString("package")
			var result any
			switch name {
			case "scan":
				progress := func(stage string) {
					if !jsonOutput {
						fmt.Fprintln(cmd.ErrOrStderr(), stage)
					}
				}
				result, err = hostsecurity.ScanHost(ctx, newServerHostSystem(), store, id, progress, serverIntelligence(server.DataDir()))
			case "status":
				result, err = store.Scan(ctx, id, scanID)
			case "report":
				severity, _ := cmd.Flags().GetString("severity")
				state, _ := cmd.Flags().GetString("state")
				var scan hostsecurity.Scan
				scan, err = store.Scan(ctx, id, scanID)
				if err == nil {
					result = buildServerReport(scan, severity, state, search, limit, offset)
				}
			case "inventory":
				result, err = store.Inventory(ctx, id, scanID, search, limit, offset)
			case "history":
				result, err = store.History(ctx, id, limit, offset)
			}
			if errors.Is(err, sql.ErrNoRows) {
				return phelixerr.New(phelixerr.CodeNotFound, "no host snapshot found; run phelix security server scan")
			}
			if err != nil {
				return err
			}
			if jsonOutput {
				return machine.WriteEnvelope(machine.Success("", result))
			}
			return printServerSecurity(cmd, result)
		}
		root.AddCommand(sub)
	}
	root.AddCommand(newServerHostConfigCommand(&jsonOutput))
	return root
}

// serverHostConfigResult is the machine view of the local host consent
// configuration.
type serverHostConfigResult struct {
	Reporting    bool   `json:"reporting"`
	ScanInterval string `json:"scan_interval"`
	Updated      bool   `json:"updated"`
}

// newServerHostConfigCommand builds the consent configuration command. The
// consent is local-only by design: it persists under the data directory and
// never adds a remote toggle.
func newServerHostConfigCommand(jsonOutput *bool) *cobra.Command {
	sub := &cobra.Command{
		Use:           "config",
		Short:         "Show or edit local host reporting consent and scan cadence",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if *jsonOutput {
				defer machine.EnterJSON()()
			}
			enable := cmd.Flags().Changed("reporting")
			disable := cmd.Flags().Changed("no-reporting")
			if enable && disable {
				return phelixerr.New(phelixerr.CodeInvalidArgument, "--reporting and --no-reporting are mutually exclusive")
			}
			interval, _ := cmd.Flags().GetString("interval")
			dataDir := server.DataDir()
			current, err := hostsecurity.LoadHostConfig(dataDir)
			if err != nil {
				return err
			}
			update := enable || disable || interval != ""
			if update {
				if enable {
					current.Reporting = true
				}
				if disable {
					current.Reporting = false
				}
				if interval != "" {
					if _, err := hostsecurity.ValidateScanInterval(interval); err != nil {
						return phelixerr.New(phelixerr.CodeInvalidArgument, err.Error())
					}
					current.ScanInterval = interval
				}
				if err := hostsecurity.SaveHostConfig(dataDir, current); err != nil {
					return err
				}
				if current, err = hostsecurity.LoadHostConfig(dataDir); err != nil {
					return err
				}
			}
			result := serverHostConfigResult{Reporting: current.Reporting, ScanInterval: current.ScanInterval, Updated: update}
			if *jsonOutput {
				return machine.WriteEnvelope(machine.Success("", result))
			}
			return printServerHostConfig(cmd, result)
		},
	}
	sub.Flags().Bool("reporting", false, "consent to local host security reporting")
	sub.Flags().Bool("no-reporting", false, "revoke host security reporting consent")
	sub.Flags().String("interval", "", "host scan cadence, e.g. 24h (minimum 1h)")
	return sub
}

// printServerHostConfig renders the consent configuration for humans.
func printServerHostConfig(cmd *cobra.Command, result serverHostConfigResult) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Host reporting\t%s\n", strconv.FormatBool(result.Reporting))
	fmt.Fprintf(w, "Scan interval\t%s\n", result.ScanInterval)
	if result.Updated {
		fmt.Fprintln(w, "State\tupdated")
	}
	return w.Flush()
}

// serverReportResult is the report payload: the filtered findings plus the
// scan provenance a consumer needs to judge them.
type serverReportResult struct {
	ScanID          string                 `json:"scan_id"`
	Status          string                 `json:"status"`
	Coverage        string                 `json:"coverage"`
	Score           securityscore.Score    `json:"score"`
	Counts          map[string]int         `json:"counts"`
	UnknownPackages int                    `json:"unknown_packages"`
	PackageCount    int                    `json:"package_count"`
	StartedAt       time.Time              `json:"started_at"`
	FinishedAt      time.Time              `json:"finished_at"`
	Warnings        []string               `json:"warnings"`
	Sources         []hostsecurity.Source  `json:"sources"`
	Findings        []hostsecurity.Finding `json:"findings"`
	Total           int                    `json:"total"`
	Limit           int                    `json:"limit"`
	Offset          int                    `json:"offset"`
}

// buildServerReport filters one scan's findings with the report flags and
// assembles the payload. Empty filters pass everything through; the scan's
// own fields are copied verbatim.
func buildServerReport(scan hostsecurity.Scan, severity, state, pkg string, limit, offset int) serverReportResult {
	filtered, total := filterServerFindings(scan.Findings, severity, state, pkg, limit, offset)
	return serverReportResult{
		ScanID:          scan.ID,
		Status:          scan.Status,
		Coverage:        scan.Coverage,
		Score:           scan.Score,
		Counts:          scan.Counts,
		UnknownPackages: scan.UnknownPackages,
		PackageCount:    scan.PackageCount,
		StartedAt:       scan.StartedAt,
		FinishedAt:      scan.FinishedAt,
		Warnings:        scan.Warnings,
		Sources:         scan.Sources,
		Findings:        filtered,
		Total:           total,
		Limit:           limit,
		Offset:          offset,
	}
}

// filterServerFindings applies the severity, state, and package filters and
// then the page bounds. Severity and state match case-insensitively and
// exactly; the package filter is a case-insensitive substring match like the
// inventory search. The total is the count of all matching findings.
func filterServerFindings(findings []hostsecurity.Finding, severity, state, pkg string, limit, offset int) ([]hostsecurity.Finding, int) {
	out := []hostsecurity.Finding{}
	for _, f := range findings {
		if severity != "" && !strings.EqualFold(f.Severity, severity) {
			continue
		}
		if state != "" && !strings.EqualFold(f.State, state) {
			continue
		}
		if pkg != "" && !strings.Contains(strings.ToLower(f.Package), strings.ToLower(pkg)) {
			continue
		}
		out = append(out, f)
	}
	total := len(out)
	if offset >= len(out) {
		return []hostsecurity.Finding{}, total
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total
}

// serverScoreLabel renders a score for humans: the value with its status, or
// unknown when the score carries no value or is itself unknown.
func serverScoreLabel(score securityscore.Score) string {
	if score.Value != nil && score.Status != "unknown" {
		return fmt.Sprintf("%d (%s)", *score.Value, score.Status)
	}
	return "unknown"
}

// serverCountsLabel renders the per-severity finding counts in a stable
// order for the human tables.
func serverCountsLabel(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
}

func printServerSecurity(cmd *cobra.Command, result any) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	switch value := result.(type) {
	case hostsecurity.Scan:
		fmt.Fprintf(w, "Scan\t%s\nStatus\t%s\nOS\t%s\nPackage manager\t%s\nPackages\t%d\nCoverage\t%s\nScore\t%s\nFinished\t%s\n", value.ID, value.Status, value.Host.Name, value.Host.Manager, value.PackageCount, value.Coverage, serverScoreLabel(value.Score), value.FinishedAt.Format(time.RFC3339))
		if len(value.Counts) > 0 {
			fmt.Fprintf(w, "Findings\t%s\n", serverCountsLabel(value.Counts))
		}
		if value.UnknownPackages > 0 {
			fmt.Fprintf(w, "Unknown packages\t%d\n", value.UnknownPackages)
		}
		for _, warning := range value.Warnings {
			fmt.Fprintf(w, "Warning\t%s\n", warning)
		}
		if value.Host.Reason != "" {
			fmt.Fprintf(w, "Availability\t%s\n", value.Host.Reason)
		}
	case serverReportResult:
		fmt.Fprintf(w, "Scan\t%s\nStatus\t%s\nCoverage\t%s\nScore\t%s\nPackages\t%d\nFinished\t%s\n", value.ScanID, value.Status, value.Coverage, serverScoreLabel(value.Score), value.PackageCount, value.FinishedAt.Format(time.RFC3339))
		if len(value.Counts) > 0 {
			fmt.Fprintf(w, "Findings\t%s\n", serverCountsLabel(value.Counts))
		}
		if value.UnknownPackages > 0 {
			fmt.Fprintf(w, "Unknown packages\t%d\n", value.UnknownPackages)
		}
		fmt.Fprintf(w, "Matching findings\t%d of %d\n", len(value.Findings), value.Total)
		for _, warning := range value.Warnings {
			fmt.Fprintf(w, "Warning\t%s\n", warning)
		}
		if len(value.Findings) > 0 {
			fmt.Fprintln(w, "")
			fmt.Fprintln(w, "SEVERITY\tADVISORY\tPACKAGE\tVERSION\tSTATE\tFIXED")
			for _, f := range value.Findings {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Severity, f.AdvisoryID, f.Package, f.Version, f.State, f.FixedVersion)
			}
		}
	case hostsecurity.Page[hostsecurity.Package]:
		fmt.Fprintln(w, "PACKAGE\tVERSION\tARCH\tSOURCE\tHELD")
		for _, p := range value.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Version, p.Architecture, p.Source, p.Held)
		}
		fmt.Fprintf(w, "Showing %d of %d packages\n", len(value.Items), value.Total)
	case hostsecurity.Page[hostsecurity.Scan]:
		fmt.Fprintln(w, "SCAN\tSTATUS\tCOVERAGE\tSCORE\tPACKAGES\tFINISHED")
		for _, s := range value.Items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n", s.ID, s.Status, s.Coverage, serverScoreLabel(s.Score), s.PackageCount, s.FinishedAt.Format(time.RFC3339))
		}
		fmt.Fprintf(w, "Showing %d of %d scans\n", len(value.Items), value.Total)
	default:
		return fmt.Errorf("unsupported result %s", strings.TrimSpace(fmt.Sprintf("%T", result)))
	}
	return w.Flush()
}
