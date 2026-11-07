package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/security"
	"github.com/abdorrahmani/phelix/internal/server"
	"github.com/abdorrahmani/phelix/internal/table"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var securityJSON bool

// SecurityCmd groups application dependency vulnerability scanning. Scans
// run locally against OSV.dev, never execute a toolchain, never modify
// dependency files, and never transmit inventory data.
var SecurityCmd = &cobra.Command{
	Use:   "security",
	Short: "Application dependency vulnerability scanning",
	Long: `Application dependency vulnerability scanning.

Inventory the Go or Rust dependencies of a project, evaluate them against
OSV.dev advisories, and review findings, history, and individual advisories.
Scans are local: dependency files are parsed in place, no toolchain runs,
and nothing is built. The advisory query sends ecosystem and package names
to OSV.dev; no dependency data reaches the Phelix backend.

Scan dispositions are conservative. A finding is affected only when the
dependency's resolved version is established (Cargo.lock, a pinned replace
directive). Go manifest requirements are minimums, so they report
potentially affected with the reasoning attached. Reachability is never
claimed: every finding reports reachability unknown.

Examples:
  phelix security scan              scan the project in the current directory
  phelix security scan myapp        scan a registered application
  phelix security report myapp      show the latest completed scan
  phelix security show GHSA-...     inspect one advisory
  phelix security config myapp      show effective scan settings`,
	Args: cobra.NoArgs,
	RunE: runSecurityMenu,
}

func init() {
	securityHistory := &cobra.Command{
		Use:           "history [AppName]",
		Short:         "List recorded vulnerability scans",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runSecurityHistory,
	}
	securityHistory.Flags().IntVar(&securityHistoryLimit, "limit", 0, "maximum number of scans to list (default: all retained)")
	securityScan := &cobra.Command{
		Use:           "scan [AppName]",
		Short:         "Scan application dependencies for known vulnerabilities",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runSecurityScan,
	}
	securityReport := &cobra.Command{
		Use:           "report [AppName]",
		Short:         "Show the latest completed vulnerability scan",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runSecurityReport,
	}
	securityReport.Flags().BoolVar(&securityReportPrevious, "previous", false, "compare with the previous completed scan")
	securityShow := &cobra.Command{
		Use:           "show [ADVISORY-ID]",
		Short:         "Show one advisory from the local cache or OSV.dev",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runSecurityShow,
	}
	securityConfig := &cobra.Command{
		Use:           "config [AppName]",
		Short:         "Show the effective vulnerability scan configuration",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runSecurityConfig,
	}
	for _, sub := range []*cobra.Command{securityScan, securityReport, securityShow, securityConfig, securityHistory} {
		sub.Flags().BoolVar(&securityJSON, "json", false, "emit the machine envelope on stdout")
		SecurityCmd.AddCommand(sub)
	}
}

var securityReportPrevious bool

// securityTarget is the resolved scan subject: a registered application or
// the project in the working directory.
type securityTarget struct {
	appID       string
	appName     string
	identityKey string
	dir         string
	cfg         *project.Config
}

// resolveSecurityTarget maps command arguments to the project to scan. A
// named argument selects a registered application by id or name; no
// argument selects the current directory's project, which must carry a
// phelix.yaml so the scan is unambiguous.
func resolveSecurityTarget(args []string) (*securityTarget, error) {
	if len(args) == 1 {
		if err := app.Manager.LoadState(); err != nil {
			return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load application state", err)
		}
		info, err := GetAppInfo(args[0])
		if err != nil {
			return nil, err
		}
		if info.Directory == "" {
			return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"application %s has no source directory recorded; rebuild it once or scan its project directory directly", info.Name)
		}
		target := &securityTarget{appID: info.ID, appName: info.Name, identityKey: info.ID, dir: info.Directory}
		if cfg, err := project.Load(info.Directory); err == nil {
			target.cfg = cfg
		}
		return target, nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "could not determine the working directory", err)
	}
	cfg, err := project.Load(dir)
	if err != nil {
		if phelixerr.CodeOf(err) == phelixerr.CodeNotFound {
			return nil, phelixerr.New(phelixerr.CodeInvalidArgument,
				"no phelix.yaml in the current directory; run from the project directory or pass an app name")
		}
		return nil, err
	}
	if err := app.Manager.LoadState(); err == nil {
		for _, info := range app.Manager.ListApplications() {
			if info.Directory == dir {
				target := &securityTarget{appID: info.ID, appName: info.Name, identityKey: info.ID, dir: dir, cfg: cfg}
				return target, nil
			}
		}
	}
	return &securityTarget{appName: cfg.Name, identityKey: projectIdentityKey(cfg.Name, dir), dir: dir, cfg: cfg}, nil
}

// projectIdentityKey builds the store identity for an unregistered
// project: the declared name plus a short digest of the absolute project
// directory, so two same-named projects never share scan history or a
// scan lock.
func projectIdentityKey(name, dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	sum := sha256.Sum256([]byte(abs))
	return "name:" + name + ":" + hex.EncodeToString(sum[:4])
}

// promptArgsWhenInteractive asks for a target application when a command
// was invoked without arguments in a terminal. Non-interactive callers
// keep their existing behavior: the current directory project or a
// precise usage error.
func promptArgsWhenInteractive(args []string, verb string) ([]string, error) {
	if len(args) > 0 || !IsInteractive() {
		return args, nil
	}
	return promptSecurityTarget(verb)
}

// effectiveThreshold resolves the configured fail threshold for a target,
// falling back to the default when no project config is available.
func effectiveThreshold(target *securityTarget) project.ScanSeverity {
	if target.cfg != nil && target.cfg.Security != nil && target.cfg.Security.Vulnerability != nil {
		return target.cfg.Security.Vulnerability.ResolvedFailThreshold()
	}
	return project.ScanSeverityDefault
}

type securityScanResult struct {
	*security.ScanRecord
	FailThreshold       string `json:"fail_threshold"`
	FindingsAtThreshold int    `json:"findings_at_threshold"`
	ThresholdTripped    bool   `json:"threshold_tripped"`
}

func runSecurityScan(cmd *cobra.Command, args []string) error {
	restore := enterSecurityJSON()
	defer restore()

	args, err := promptArgsWhenInteractive(args, "scan")
	if err != nil {
		return err
	}
	target, err := resolveSecurityTarget(args)
	if err != nil {
		return err
	}

	store := security.NewScanStore(server.DataDir())
	lock, err := store.AcquireScanLock(target.identityKey)
	if err != nil {
		return err
	}
	defer lock.Release()

	opts := security.ScanOptions{
		AppID:       target.appID,
		AppName:     target.appName,
		IdentityKey: target.identityKey,
		Dir:         target.dir,
		Trigger:     security.TriggerManual,
		DataDir:     server.DataDir(),
		OSVBaseURL:  security.OSVBaseURL(),
		Progress:    newSecurityProgress(),
	}
	result, err := security.RunScan(context.Background(), opts)
	finishSecurityProgress()
	if err != nil {
		return err
	}

	record := result.Record
	threshold := effectiveThreshold(target)
	tripped, atThreshold := record.Counts.AtOrAbove(string(threshold))

	partial := record.Coverage == security.CoveragePartial
	if securityJSON {
		if writeErr := machine.WriteEnvelope(machine.Success(record.ScanID, securityScanResult{
			ScanRecord:          record,
			FailThreshold:       string(threshold),
			FindingsAtThreshold: atThreshold,
			ThresholdTripped:    tripped,
		})); writeErr != nil {
			return writeErr
		}
		if err := incompleteScanResult(tripped, partial, record, atThreshold, threshold); err != nil {
			machine.LeaveJSON()
			return err
		}
		return nil
	}

	renderScanHuman(record, threshold, tripped, atThreshold)
	if err := incompleteScanResult(tripped, partial, record, atThreshold, threshold); err != nil {
		return err
	}
	return nil
}

// incompleteScanResult decides the exit semantics of a scan that executed
// but cannot present itself as a complete clean check. Findings at the
// threshold dominate (exit 25); otherwise partial advisory coverage fails
// closed (exit 30) so a CI gate can never pass on an unchecked project.
func incompleteScanResult(tripped, partial bool, record *security.ScanRecord, atThreshold int, threshold project.ScanSeverity) error {
	if tripped {
		return phelixerr.Newf(phelixerr.CodeSecurityFindings,
			"%d finding(s) at or above the %s threshold; scan record %s", atThreshold, threshold, record.ScanID)
	}
	if partial {
		return phelixerr.Newf(phelixerr.CodeConnection,
			"advisory coverage is incomplete and no findings were reported; rerun with network access to check every package (scan record %s)", record.ScanID)
	}
	return nil
}

// renderScanHuman prints the human scan view. Machine mode redirects
// stdout to stderr, so progress-style output stays out of the envelope.
func renderScanHuman(record *security.ScanRecord, threshold project.ScanSeverity, tripped bool, atThreshold int) {
	fmt.Printf("Scan %s of %s (%s inventory)\n", record.ScanID, record.AppName, inventoryLevel(record))
	if record.Inventory != nil {
		fmt.Printf("Modules: %d inventoried, %d matchable\n", record.Counts.Modules, record.Counts.Matchable)
	}
	if !record.StartedAt.IsZero() && record.FinishedAt != nil {
		fmt.Printf("Completed in %s\n", record.FinishedAt.Sub(record.StartedAt).Round(time.Millisecond))
	}
	for _, warning := range record.Warnings {
		fmt.Printf("%s %s\n", color.YellowString("⚠"), warning)
	}
	if record.Freshness == security.FreshnessStale {
		fmt.Printf("%s advisory data may be out of date\n", color.YellowString("⚠"))
	}

	if len(record.Findings) == 0 {
		fmt.Printf("%s No open findings; %d advisories evaluated as not affected.\n",
			color.GreenString("✓"), record.Counts.NotAffected)
	} else {
		fmt.Println()
		printFindingsTable(record.Findings)
		fmt.Println()
		for _, finding := range record.Findings {
			if finding.Summary != "" {
				fmt.Printf("%s %s: %s\n", stateSymbol(finding.State), finding.AdvisoryID, finding.Summary)
			}
		}
	}

	if tripped {
		fmt.Printf("\n%s %d finding(s) at or above the %s threshold\n",
			color.RedString("✗"), atThreshold, threshold)
	}
}

// printFindingsTable renders the shared findings table. Long package
// identities truncate; the full value stays available in JSON and in
// report output.
func printFindingsTable(findings []security.Finding) {
	t := table.New(os.Stdout, false)
	t.Header([]string{"Severity", "Advisory", "Package", "Version", "State"})
	for _, finding := range findings {
		t.Append([]string{
			finding.Severity,
			finding.AdvisoryID,
			truncateCell(finding.Package, 44),
			truncateCell(strings.TrimPrefix(finding.Version, "v"), 20),
			finding.State,
		})
	}
	_ = t.Render()
}

// truncateCell bounds a cell value and marks the cut so wide identities
// cannot stretch a table past readability.
func truncateCell(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max <= 1 {
		return "..."
	}
	return string(runes[:max-1]) + "…"
}

func inventoryLevel(record *security.ScanRecord) string {
	if record.Inventory == nil {
		return "unknown"
	}
	return string(record.Inventory.Level)
}

func stateSymbol(state string) string {
	switch state {
	case security.StateAffected:
		return color.RedString("✗")
	case security.StatePotentiallyAffected:
		return color.YellowString("⚠")
	case security.StateUnknown:
		return color.YellowString("?")
	case security.StateResolved:
		return color.GreenString("✓")
	default:
		return " "
	}
}

type securityResolvedFinding struct {
	AdvisoryID   string `json:"advisory_id"`
	Package      string `json:"package"`
	Version      string `json:"version,omitempty"`
	FixedVersion string `json:"fixed_version,omitempty"`
}

type securityReportResult struct {
	Scan           *security.ScanRecord      `json:"scan"`
	PreviousScanID string                    `json:"previous_scan_id,omitempty"`
	NewlyDetected  []string                  `json:"newly_detected,omitempty"`
	Persistent     []string                  `json:"persistent,omitempty"`
	Resolved       []securityResolvedFinding `json:"resolved,omitempty"`
}

func runSecurityReport(cmd *cobra.Command, args []string) error {
	restore := enterSecurityJSON()
	defer restore()

	args, err := promptArgsWhenInteractive(args, "view the report for")
	if err != nil {
		return err
	}
	target, err := resolveSecurityTarget(args)
	if err != nil {
		return err
	}
	store := security.NewScanStore(server.DataDir())
	entry, err := store.Latest(target.identityKey)
	if err != nil {
		return err
	}
	if entry == nil || entry.ScanID == "" {
		return phelixerr.Newf(phelixerr.CodeNotFound,
			"no completed scan found for %s; run phelix security scan first", target.appName)
	}
	record, err := store.LoadScan(entry.ScanID)
	if err != nil {
		return err
	}

	result := securityReportResult{Scan: record}
	if securityReportPrevious {
		previous, err := previousCompletedScan(store, target.identityKey, record)
		if err != nil {
			return err
		}
		if previous != nil {
			result.PreviousScanID = previous.ScanID
			result.NewlyDetected, result.Persistent, result.Resolved = diffFindings(previous, record)
		}
	}

	if securityJSON {
		return machine.WriteEnvelope(machine.Success(record.ScanID, result))
	}
	renderReportHuman(record, result)
	return nil
}

// previousCompletedScan returns the most recent completed scan older than
// the given one.
func previousCompletedScan(store *security.ScanStore, identityKey string, current *security.ScanRecord) (*security.ScanRecord, error) {
	records, err := store.ListScans(identityKey)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.Status != security.ScanCompleted || record.ScanID == current.ScanID {
			continue
		}
		if record.StartedAt.Before(current.StartedAt) {
			return record, nil
		}
	}
	return nil, nil
}

// findingKey identifies a finding across scans.
func findingKey(finding security.Finding) string {
	return finding.AdvisoryID + "\x00" + finding.Package
}

// diffFindings compares two completed scans by advisory and package.
func diffFindings(previous, current *security.ScanRecord) (newly, persistent []string, resolved []securityResolvedFinding) {
	prevSet := map[string]security.Finding{}
	for _, finding := range previous.Findings {
		prevSet[findingKey(finding)] = finding
	}
	currentSet := map[string]bool{}
	for _, finding := range current.Findings {
		currentSet[findingKey(finding)] = true
	}
	for _, finding := range current.Findings {
		key := findingKey(finding)
		if _, ok := prevSet[key]; ok {
			persistent = append(persistent, key)
		} else {
			newly = append(newly, key)
		}
	}
	for key, finding := range prevSet {
		if !currentSet[key] {
			resolved = append(resolved, securityResolvedFinding{
				AdvisoryID:   finding.AdvisoryID,
				Package:      finding.Package,
				Version:      finding.Version,
				FixedVersion: finding.FixedVersion,
			})
		}
	}
	sort.Strings(newly)
	sort.Strings(persistent)
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].AdvisoryID < resolved[j].AdvisoryID })
	return newly, persistent, resolved
}

func renderReportHuman(record *security.ScanRecord, result securityReportResult) {
	fmt.Printf("Latest scan %s of %s (%s)\n", record.ScanID, record.AppName, record.Status)
	fmt.Printf("Advisory data: %s", record.Freshness)
	if record.Coverage != "" {
		fmt.Printf(", coverage: %s", record.Coverage)
	}
	fmt.Println()
	for _, warning := range record.Warnings {
		fmt.Printf("%s %s\n", color.YellowString("⚠"), warning)
	}
	if len(record.Findings) == 0 {
		fmt.Printf("%s No open findings; %d advisories evaluated as not affected.\n",
			color.GreenString("✓"), record.Counts.NotAffected)
	} else {
		fmt.Println()
		printFindingsTable(record.Findings)
	}
	if result.PreviousScanID != "" {
		fmt.Printf("\nCompared with %s: %d newly detected, %d persistent, %d resolved\n",
			result.PreviousScanID, len(result.NewlyDetected), len(result.Persistent), len(result.Resolved))
		for _, resolved := range result.Resolved {
			fmt.Printf("%s %s on %s is resolved\n", stateSymbol(security.StateResolved), resolved.AdvisoryID, resolved.Package)
		}
	}
}

type securityConfigResult struct {
	AppName       string     `json:"app_name"`
	AppID         string     `json:"app_id,omitempty"`
	Enabled       bool       `json:"enabled"`
	ScanInterval  string     `json:"scan_interval"`
	FailThreshold string     `json:"fail_threshold"`
	NextScanAt    *time.Time `json:"next_scan_at,omitempty"`
	LastScanID    string     `json:"last_scan_id,omitempty"`
	LastScanAt    *time.Time `json:"last_scan_at,omitempty"`
}

func runSecurityConfig(cmd *cobra.Command, args []string) error {
	restore := enterSecurityJSON()
	defer restore()

	args, err := promptArgsWhenInteractive(args, "configure")
	if err != nil {
		return err
	}
	target, err := resolveSecurityTarget(args)
	if err != nil {
		return err
	}
	if target.cfg == nil {
		return phelixerr.Newf(phelixerr.CodeNotFound,
			"%s has no phelix.yaml; there is no scan configuration to show", target.appName)
	}

	enabled := false
	interval := project.ScanIntervalDefault
	threshold := project.ScanSeverityDefault
	if target.cfg.Security != nil && target.cfg.Security.Vulnerability != nil {
		v := target.cfg.Security.Vulnerability
		enabled = v.Enabled
		if resolved, err := v.ResolvedScanInterval(); err == nil {
			interval = resolved
		}
		threshold = v.ResolvedFailThreshold()
	}

	result := securityConfigResult{
		AppName:       target.appName,
		AppID:         target.appID,
		Enabled:       enabled,
		ScanInterval:  interval.String(),
		FailThreshold: string(threshold),
	}
	store := security.NewScanStore(server.DataDir())
	if entry, err := store.Latest(target.identityKey); err == nil && entry != nil {
		result.NextScanAt = entry.NextScanAt
		result.LastScanID = entry.ScanID
		result.LastScanAt = entry.FinishedAt
	}

	if securityJSON {
		return machine.WriteEnvelope(machine.Success("security-config", result))
	}
	fmt.Printf("Vulnerability scanning for %s\n", target.appName)
	t := table.New(os.Stdout, false)
	t.Header([]string{"Setting", "Value"})
	t.Append([]string{"Enabled", strconv.FormatBool(result.Enabled)})
	t.Append([]string{"Scan interval", result.ScanInterval})
	t.Append([]string{"Fail threshold", result.FailThreshold})
	if result.LastScanID != "" {
		t.Append([]string{"Last completed scan", result.LastScanID})
	}
	if result.NextScanAt != nil {
		t.Append([]string{"Next scheduled scan", result.NextScanAt.Format(time.RFC3339)})
	}
	_ = t.Render()
	return nil
}

type securityShowResult struct {
	Advisory *security.OSVAdvisory `json:"advisory"`
	Cached   bool                  `json:"cached"`
}

func runSecurityShow(cmd *cobra.Command, args []string) error {
	restore := enterSecurityJSON()
	defer restore()

	if len(args) == 0 {
		if !IsInteractive() {
			return phelixerr.New(phelixerr.CodeInvalidArgument,
				"missing required <ADVISORY-ID>; usage: phelix security show <ADVISORY-ID>")
		}
		id, err := PromptString("Advisory identifier (e.g. GHSA-xxxx-xxxx-xxxx):", "")
		if err != nil {
			return err
		}
		if id == "" {
			return phelixerr.New(phelixerr.CodeInvalidArgument, "an advisory identifier is required")
		}
		args = []string{id}
	}
	id := strings.TrimSpace(args[0])
	store := security.NewScanStore(server.DataDir())
	cache := security.NewAdvisoryCache(store.AdvisoryCacheDir())

	adv, _, err := cache.Get(id)
	cached := err == nil && adv != nil
	if adv == nil {
		client := &security.OSVClient{BaseURL: security.OSVBaseURL()}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		adv, err = client.FetchAdvisory(ctx, id)
		if err != nil {
			if cached {
				return err
			}
			return phelixerr.Wrap(phelixerr.CodeConnection,
				"advisory is not cached and OSV.dev could not be reached", err)
		}
		_ = cache.Put(adv, time.Now())
	}

	if securityJSON {
		return machine.WriteEnvelope(machine.Success(adv.ID, securityShowResult{Advisory: adv, Cached: cached}))
	}
	renderAdvisoryHuman(adv, cached)
	return nil
}

// renderAdvisoryHuman prints a bounded plain-text advisory view. Advisory
// content is untrusted: fields are printed as single-line text with
// control characters removed.
func renderAdvisoryHuman(adv *security.OSVAdvisory, cached bool) {
	fmt.Printf("%s", adv.ID)
	if cached {
		fmt.Printf(" (from cache)")
	}
	fmt.Println()
	if len(adv.Aliases) > 0 {
		fmt.Printf("Also known as: %s\n", strings.Join(adv.Aliases, ", "))
	}
	label, _, vectors := advisorySeverityLabel(adv)
	fmt.Printf("Severity: %s\n", label)
	for _, vector := range vectors {
		fmt.Printf("CVSS: %s\n", vector)
	}
	if adv.Summary != "" {
		fmt.Printf("Summary: %s\n", sanitizeText(adv.Summary))
	}
	for _, affected := range adv.Affected {
		fmt.Printf("Affects %s %s\n", affected.Package.Ecosystem, affected.Package.Name)
		for _, rng := range affected.Ranges {
			fmt.Printf("  %s\n", rangeSummary(rng))
		}
	}
	if len(adv.References) > 0 {
		fmt.Println("References:")
		for _, ref := range adv.References {
			fmt.Printf("  %s\n", ref.URL)
		}
	}
	fmt.Printf("Details: https://osv.dev/vulnerability/%s\n", adv.ID)
}

// sanitizeText strips control characters from untrusted advisory text.
func sanitizeText(text string) string {
	var out strings.Builder
	for _, r := range text {
		if r < 0x20 && r != '\t' {
			continue
		}
		out.WriteRune(r)
	}
	const maxText = 500
	result := out.String()
	if len(result) > maxText {
		result = result[:maxText]
	}
	return result
}

// rangeSummary renders one OSV range for human output.
func rangeSummary(rng security.OSVRange) string {
	introduced, fixed, limit := "", "", ""
	for _, event := range rng.Events {
		if v, ok := event["introduced"]; ok {
			introduced = v
		}
		if v, ok := event["fixed"]; ok {
			fixed = v
		}
		if v, ok := event["limit"]; ok {
			limit = v
		}
		if v, ok := event["last_affected"]; ok {
			fixed = "≤ " + v
		}
	}
	switch {
	case fixed != "":
		return fmt.Sprintf("%s: introduced %s, fixed %s", rng.Type, introduced, fixed)
	case limit != "":
		return fmt.Sprintf("%s: introduced %s, reviewed up to %s", rng.Type, introduced, limit)
	default:
		return fmt.Sprintf("%s: introduced %s, no fix", rng.Type, introduced)
	}
}

// advisorySeverityLabel reuses the matcher's documented OSV mapping for
// display.
func advisorySeverityLabel(adv *security.OSVAdvisory) (string, string, []string) {
	var vectors []string
	for _, severity := range adv.Severity {
		if severity.Type == "CVSS_V3" || severity.Type == "CVSS_V4" {
			vectors = append(vectors, severity.Score)
		}
	}
	if raw, ok := adv.DatabaseSpecific["severity"].(string); ok && raw != "" {
		return strings.ToLower(raw), "osv database_specific", vectors
	}
	for _, affected := range adv.Affected {
		if raw, ok := affected.DatabaseSpecific["severity"].(string); ok && raw != "" {
			return strings.ToLower(raw), "osv database_specific", vectors
		}
	}
	return security.SeverityUnknown, "", vectors
}

// enterSecurityJSON switches the process into machine mode when --json was
// requested, keeping stdout reserved for the envelope.
func enterSecurityJSON() func() {
	if securityJSON {
		return machine.EnterJSON()
	}
	return func() {}
}
