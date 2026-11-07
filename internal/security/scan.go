package security

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// ScanTimeout bounds one scan end to end. Advisory retrieval is bounded
// per request; this deadline keeps the whole scan, including inventory
// parsing, from running away.
const ScanTimeout = 90 * time.Second

// ScanOptions describes one scan execution.
type ScanOptions struct {
	// AppID and AppName identify the scanned application. IdentityKey
	// attributes the scan in the store: the app id when the project is a
	// registered app, a name-based key otherwise.
	AppID       string
	AppName     string
	IdentityKey string
	// Dir is the project directory holding the dependency files.
	Dir string
	// Trigger records what started the scan.
	Trigger      ScanTrigger
	ScheduledFor *time.Time
	// DataDir is the Phelix data directory holding the security store.
	DataDir string
	// HTTPClient and OSVBaseURL are injectable for tests.
	HTTPClient *http.Client
	OSVBaseURL string
	// Now makes the clock injectable for tests.
	Now func() time.Time
}

// ScanResult couples the persisted record with its open findings.
type ScanResult struct {
	Record *ScanRecord
	Open   []Finding
}

// RunScan inventories the project, matches it against OSV.dev through the
// advisory cache, and persists the scan record. The per-app scan lock must
// be held by the caller.
func RunScan(ctx context.Context, opts ScanOptions) (*ScanResult, error) {
	if opts.IdentityKey == "" {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "a scan needs an app identity")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ScanTimeout)
		defer cancel()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	store := NewScanStore(opts.DataDir)
	store.Now = now
	cache := NewAdvisoryCache(store.AdvisoryCacheDir())
	cache.Now = now
	client := &OSVClient{BaseURL: opts.OSVBaseURL, HTTP: opts.HTTPClient}

	started := now()
	record := &ScanRecord{
		ScanID:      NewScanID(),
		AppID:       opts.AppID,
		AppName:     opts.AppName,
		IdentityKey: opts.IdentityKey,
		Trigger:     opts.Trigger,
		Status:      ScanRunning,
		StartedAt:   started,
		Freshness:   FreshnessFresh,
	}
	record.ScheduledFor = opts.ScheduledFor
	if err := store.SaveScan(record); err != nil {
		return nil, err
	}
	fail := func(cause error) (*ScanResult, error) {
		stamp := now()
		record.Status = ScanFailed
		record.FinishedAt = &stamp
		record.Outcome = sanitizeOutcome(cause)
		if saveErr := store.SaveScan(record); saveErr != nil {
			return nil, cause
		}
		return nil, cause
	}

	eco, err := DetectEcosystem(opts.Dir)
	if err != nil {
		return fail(err)
	}
	var inv *Inventory
	switch eco {
	case EcosystemGo:
		inv, err = InventoryGo(opts.Dir)
	case EcosystemRust:
		inv, err = InventoryRust(opts.Dir)
	}
	if err != nil {
		return fail(err)
	}
	record.Inventory = inv
	record.SourceFiles = FingerprintFiles(opts.Dir, fingerprintNames(eco))

	source := NewPrefetchSource(cache, client)
	source.Now = now
	if err := source.Prefetch(ctx, prefetchQueries(inv)); err != nil {
		return fail(err)
	}
	if source.NetworkFailed() && !source.HasData() {
		return fail(phelixerr.New(phelixerr.CodeConnection,
			"OSV.dev is unreachable and no cached advisory data exists for this project"))
	}

	findings, err := MatchInventory(ctx, inv, source)
	if err != nil {
		return fail(err)
	}

	freshness := FreshnessFresh
	if source.StaleUsed() || source.NetworkFailed() {
		freshness = FreshnessStale
	}
	open, counts, warnings := buildCounts(inv, findings)
	for _, note := range inv.Notes {
		warnings = append(warnings, note)
	}
	if source.NetworkFailed() {
		warnings = append(warnings, "OSV.dev could not be reached; results use locally cached advisory data")
	} else if source.StaleUsed() {
		warnings = append(warnings, "some advisory data is past its freshness window")
	}
	if failures := source.FetchFailures(); failures > 0 {
		warnings = append(warnings, "some advisories could not be retrieved")
	}

	finished := now()
	record.Status = ScanCompleted
	record.FinishedAt = &finished
	record.Findings = open
	record.Counts = counts
	record.Freshness = freshness
	record.Warnings = warnings
	if err := store.SaveScan(record); err != nil {
		return nil, err
	}
	if err := store.Retain(opts.IdentityKey, DefaultScanRetention); err != nil {
		return nil, err
	}
	if err := store.MarkLatestSuccess(opts.IdentityKey, record.ScanID, finished); err != nil {
		return nil, err
	}
	return &ScanResult{Record: record, Open: open}, nil
}

// sanitizeOutcome bounds a failure message, mapping cancellation and
// deadlines to operator language and stripping local path detail.
func sanitizeOutcome(cause error) string {
	switch {
	case cause == nil:
		return "scan failed"
	case errors.Is(cause, context.Canceled):
		return "scan cancelled"
	case errors.Is(cause, context.DeadlineExceeded):
		return "scan deadline exceeded"
	}
	message := cause.Error()
	const maxOutcome = 300
	if len(message) > maxOutcome {
		message = message[:maxOutcome]
	}
	return message
}

// prefetchQueries collects one query per distinct package identity.
func prefetchQueries(inv *Inventory) []OSVQuery {
	seen := map[string]bool{}
	var out []OSVQuery
	for _, mod := range inv.Modules {
		if !mod.Matchable {
			continue
		}
		if seen[mod.Name] {
			continue
		}
		seen[mod.Name] = true
		out = append(out, OSVQuery{Ecosystem: inv.Ecosystem, Name: mod.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fingerprintNames lists the dependency files relevant to an ecosystem.
func fingerprintNames(eco Ecosystem) []string {
	if eco == EcosystemGo {
		return []string{"go.mod", "go.sum", "go.work"}
	}
	return []string{"Cargo.toml", "Cargo.lock"}
}

// buildCounts splits evaluated findings into open findings and summary
// counts, applying the stored-findings bound.
func buildCounts(inv *Inventory, findings []Finding) ([]Finding, ScanCounts, []string) {
	counts := ScanCounts{
		Modules:    len(inv.Modules),
		Matchable:  0,
		Evaluated:  len(findings),
		BySeverity: map[string]int{},
	}
	for _, mod := range inv.Modules {
		if mod.Matchable {
			counts.Matchable++
		}
	}
	var open []Finding
	truncated := false
	for _, finding := range findings {
		counts.BySeverity[finding.Severity]++
		switch finding.State {
		case StateAffected:
			counts.Affected++
			open = append(open, finding)
		case StatePotentiallyAffected:
			counts.PotentiallyAffected++
			open = append(open, finding)
		case StateUnknown:
			counts.Unknown++
			open = append(open, finding)
		case StateNotAffected:
			counts.NotAffected++
		}
	}
	if len(open) > MaxStoredFindings {
		open = open[:MaxStoredFindings]
		truncated = true
	}
	var warnings []string
	if truncated {
		warnings = append(warnings, "more findings than the storage bound; the record keeps the first 1000 sorted findings")
	}
	return open, counts, warnings
}
