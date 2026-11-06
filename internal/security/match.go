package security

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Finding states. resolved is report-time only: it describes a finding of
// a previous scan that no longer applies, and is never stored as a
// current advisory disposition.
const (
	StateAffected            = "affected"
	StatePotentiallyAffected = "potentially_affected"
	StateNotAffected         = "not_affected"
	StateUnknown             = "unknown"
	StateResolved            = "resolved"
)

// Severity vocabulary. unknown is its own value, never a rank between the
// known ones.
const (
	SeverityLow      = "low"
	SeverityMedium   = "medium"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
	SeverityUnknown  = "unknown"
)

// ReachabilityUnknown is the only reachability value Phase 1 reports:
// matching never determines whether vulnerable code is compiled in or
// called.
const ReachabilityUnknown = "unknown"

// FindingSourceOSV marks advisories retrieved from OSV.dev.
const FindingSourceOSV = "osv"

// SeverityRank orders known severities for threshold comparison. Unknown
// severity ranks below every known one; threshold policy treats it as its
// own documented case.
func SeverityRank(severity string) int {
	switch severity {
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	case SeverityHigh:
		return 3
	case SeverityCritical:
		return 4
	default:
		return 0
	}
}

// AdvisoryEntry couples an advisory with its retrieval provenance.
type AdvisoryEntry struct {
	Advisory  *OSVAdvisory
	FetchedAt time.Time
	Stale     bool
}

// AdvisorySource supplies every advisory known for a package identity.
type AdvisorySource interface {
	Advisories(ctx context.Context, eco Ecosystem, name string) ([]AdvisoryEntry, error)
}

// Finding is one advisory evaluated against one dependency. Every field a
// consumer needs to audit the decision is present: identity, evidence,
// disposition, and provenance.
type Finding struct {
	AdvisoryID       string     `json:"advisory_id"`
	Aliases          []string   `json:"aliases,omitempty"`
	Related          []string   `json:"related,omitempty"`
	Package          string     `json:"package"`
	Ecosystem        Ecosystem  `json:"ecosystem"`
	Version          string     `json:"version,omitempty"`
	VersionResolved  bool       `json:"version_resolved"`
	Direct           bool       `json:"direct"`
	Dev              bool       `json:"dev,omitempty"`
	Optional         bool       `json:"optional,omitempty"`
	State            string     `json:"state"`
	Reason           string     `json:"reason"`
	FixedVersion     string     `json:"fixed_version,omitempty"`
	Ranges           []string   `json:"ranges,omitempty"`
	Severity         string     `json:"severity"`
	SeveritySource   string     `json:"severity_source,omitempty"`
	CVSSVectors      []string   `json:"cvss_vectors,omitempty"`
	Summary          string     `json:"summary,omitempty"`
	References       []string   `json:"references,omitempty"`
	URL              string     `json:"url"`
	Source           string     `json:"source"`
	RetrievedAt      time.Time  `json:"retrieved_at,omitempty"`
	AdvisoryModified string     `json:"advisory_modified,omitempty"`
	Reachability     string     `json:"reachability"`
	Confidence       Confidence `json:"confidence"`
}

// MatchInventory evaluates every matchable module of an inventory against
// the advisory source and returns one finding per advisory-package pair.
// The result includes not_affected dispositions so callers can report
// complete evaluation coverage; open findings are the caller's filter.
func MatchInventory(ctx context.Context, inv *Inventory, source AdvisorySource) ([]Finding, error) {
	queries := map[string]bool{}
	var ordered []OSVQuery
	byModule := map[string][]AdvisoryEntry{}

	for _, mod := range inv.Modules {
		if !mod.Matchable {
			continue
		}
		key := string(inv.Ecosystem) + "\x00" + mod.Name
		if !queries[key] {
			queries[key] = true
			ordered = append(ordered, OSVQuery{Ecosystem: inv.Ecosystem, Name: mod.Name})
		}
	}
	for _, query := range ordered {
		entries, err := source.Advisories(ctx, query.Ecosystem, query.Name)
		if err != nil {
			return nil, err
		}
		byModule[queryKey(query.Ecosystem, query.Name)] = entries
	}

	var findings []Finding
	for _, mod := range inv.Modules {
		if !mod.Matchable {
			continue
		}
		entries := byModule[queryKey(inv.Ecosystem, mod.Name)]
		var drafts []Finding
		for _, entry := range entries {
			finding := evaluateModuleAdvisory(inv.Ecosystem, mod, entry)
			if finding != nil {
				drafts = append(drafts, *finding)
			}
		}
		findings = append(findings, mergeDrafts(drafts)...)
	}

	sortFindings(findings)
	return findings, nil
}

func queryKey(eco Ecosystem, name string) string {
	return string(eco) + "\x00" + name
}

// evaluateModuleAdvisory produces one finding draft for one advisory, or
// nil when the advisory does not describe this package.
func evaluateModuleAdvisory(eco Ecosystem, mod Module, entry AdvisoryEntry) *Finding {
	adv := entry.Advisory
	relevant := 0
	for _, affected := range adv.Affected {
		if affectedMatches(affected, eco, mod.Name) {
			relevant++
		}
	}
	if relevant == 0 {
		return nil
	}

	compare := ecosystemCompare(eco)
	var inRanges, aboveRanges, belowRanges, uncertainRanges []string
	explicitHit := false
	fixed := osvFixedVersion(adv, eco, mod.Name)

	for _, affected := range adv.Affected {
		if !affectedMatches(affected, eco, mod.Name) {
			continue
		}
		for _, version := range affected.Versions {
			if evidenceMentions(mod, version, compare) {
				explicitHit = true
			}
		}
		for _, rng := range affected.Ranges {
			verdict, summary := evaluateModuleRange(mod, rng, compare)
			switch {
			case verdict == verdictIn:
				inRanges = append(inRanges, summary)
			case verdict == verdictOutAbove:
				aboveRanges = append(aboveRanges, summary)
			case verdict == verdictOutBelow:
				belowRanges = append(belowRanges, summary)
			default:
				uncertainRanges = append(uncertainRanges, summary)
			}
		}
	}

	decision := decideState(mod, inRanges, aboveRanges, belowRanges, uncertainRanges, explicitHit)
	if decision == nil {
		return nil
	}
	decision.AdvisoryID = adv.ID
	decision.Package = mod.Name
	decision.Ecosystem = eco
	decision.FixedVersion = fixed
	decision.Ranges = append(inRanges, append(aboveRanges, append(belowRanges, uncertainRanges...)...)...)
	decision.AdvisoryModified = adv.Modified
	decision.RetrievedAt = entry.FetchedAt
	decision.Source = FindingSourceOSV
	decision.URL = "https://osv.dev/vulnerability/" + adv.ID
	decision.Reachability = ReachabilityUnknown
	decision.Confidence = mod.Confidence
	decision.Direct = mod.Direct
	decision.Dev = mod.Dev
	decision.Optional = mod.Optional
	switch mod.Confidence {
	case ConfidenceResolved:
		decision.Version, decision.VersionResolved = mod.Version, true
	case ConfidenceDeclaredMinimum, ConfidenceObserved:
		decision.Version = mod.Version
	default:
		decision.Version = ""
	}
	label, source, vectors := advisorySeverity(adv, eco, mod.Name)
	decision.Severity = label
	decision.SeveritySource = source
	decision.CVSSVectors = vectors
	decision.Summary = advisorySummary(adv)
	decision.References = advisoryReferences(adv)
	decision.Aliases = sortStrings(adv.Aliases)
	decision.Related = sortStrings(adv.Related)
	return decision
}

// evaluateModuleRange evaluates one range against the module's evidence,
// choosing the comparison path that matches the confidence level.
func evaluateModuleRange(mod Module, rng OSVRange, compare compareVersion) (rangeVerdict, string) {
	if rng.Type == "GIT" {
		return verdictUnevaluable, "(git range; not evaluable from dependency files)"
	}
	if rng.Type != "SEMVER" && rng.Type != "ECOSYSTEM" {
		return verdictUnevaluable, "(unsupported range type " + rng.Type + ")"
	}

	if mod.Confidence == ConfidenceRequirement {
		return evaluateRequirementRange(mod, rng, compare)
	}

	candidates := []string{mod.Version}
	if mod.Confidence == ConfidenceObserved {
		candidates = mod.ObservedVersions
	}
	if len(candidates) == 0 {
		return verdictUnevaluable, "(no version evidence)"
	}

	summary := ""
	verdict := verdictOutAbove
	for i, candidate := range candidates {
		v, s, err := evaluateOSVRange(rng, candidate, compare)
		if err != nil {
			return verdictUnevaluable, "(unevaluable version evidence)"
		}
		summary = s
		if i == 0 || rankVerdict(v) < rankVerdict(verdict) {
			verdict = v
		}
	}
	return verdict, summary
}

// evaluateRequirementRange intersects a manifest requirement interval with
// an advisory range. A disjoint intersection is a confident exclusion: the
// selected version always lies inside the requirement interval.
func evaluateRequirementRange(mod Module, rng OSVRange, compare compareVersion) (rangeVerdict, string) {
	req, err := parseCargoRequirement(mod.Requirement)
	if err != nil {
		return verdictUnevaluable, "(requirement " + mod.Requirement + " cannot be evaluated)"
	}
	segments, ok := parseOSVRangeEvents(rng.Events)
	if !ok {
		return verdictUnevaluable, "(range carries no introduced event)"
	}
	summary := osvRangeSummary(rng.Type, segments)

	overlap := false
	uncertain := false
	for _, segment := range segments {
		segmentIv, err := segmentInterval(segment)
		if err != nil {
			return verdictUnevaluable, "(range boundaries cannot be evaluated)"
		}
		if cargoOverlaps(segmentIv, req) {
			if segment.limit != "" {
				uncertain = true
				continue
			}
			overlap = true
		}
	}
	switch {
	case overlap:
		return verdictIn, summary
	case uncertain:
		return verdictBeyondLimit, summary
	default:
		return verdictOutAbove, summary
	}
}

// segmentInterval converts one introduced..upper segment into a version
// interval for interval arithmetic.
func segmentInterval(segment osvSegment) (cargoInterval, error) {
	lo, ok := parseCargoVersionLoose(segment.introduced)
	if !ok {
		return cargoInterval{}, fmt.Errorf("unparsable introduced boundary %q", segment.introduced)
	}
	iv := cargoInterval{lo: lo, loInc: true, unboundedAbove: true}
	switch {
	case segment.fixed != "":
		hi, ok := parseCargoVersionLoose(segment.fixed)
		if !ok {
			return cargoInterval{}, fmt.Errorf("unparsable fixed boundary %q", segment.fixed)
		}
		iv.hi, iv.unboundedAbove = hi, false
	case segment.lastAffected != "":
		hi, ok := parseCargoVersionLoose(segment.lastAffected)
		if !ok {
			return cargoInterval{}, fmt.Errorf("unparsable last_affected boundary %q", segment.lastAffected)
		}
		iv.hi, iv.hiInc, iv.unboundedAbove = hi, true, false
	case segment.limit != "":
		hi, ok := parseCargoVersionLoose(segment.limit)
		if !ok {
			return cargoInterval{}, fmt.Errorf("unparsable limit boundary %q", segment.limit)
		}
		iv.hi, iv.unboundedAbove = hi, false
	}
	return iv, nil
}

// parseCargoVersionLoose parses a boundary version. The OSV "0" floor is
// represented as a zero version that compares below everything in the
// interval arithmetic used here.
func parseCargoVersionLoose(value string) (cargoVer, bool) {
	if value == "0" {
		return cargoVer{}, true
	}
	return parseCargoVersion(value)
}

// evidenceMentions reports whether the module's version evidence names an
// explicit affected version.
func evidenceMentions(mod Module, version string, compare compareVersion) bool {
	candidates := []string{mod.Version}
	if mod.Confidence == ConfidenceObserved {
		candidates = mod.ObservedVersions
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if cmp, err := compare(candidate, version); err == nil && cmp == 0 {
			return true
		}
	}
	return false
}

// decideState applies the confidence-specific decision matrix.
func decideState(mod Module, in, above, below, uncertain []string, explicitHit bool) *Finding {
	switch mod.Confidence {
	case ConfidenceResolved:
		switch {
		case explicitHit || len(in) > 0:
			return &Finding{State: StateAffected, Reason: fmt.Sprintf("resolved version %s is inside the affected range", mod.Version)}
		case len(uncertain) > 0:
			return &Finding{State: StateUnknown, Reason: "the advisory carries a range that cannot be evaluated for this version"}
		default:
			return &Finding{State: StateNotAffected, Reason: fmt.Sprintf("resolved version %s lies outside every affected range", mod.Version)}
		}
	case ConfidenceDeclaredMinimum:
		switch {
		case len(in) > 0:
			return &Finding{State: StatePotentiallyAffected, Reason: fmt.Sprintf("declared minimum %s lies inside the affected range; the selected version is at least %s", mod.Version, mod.Version)}
		case len(uncertain) > 0 || len(below) > 0:
			return &Finding{State: StateUnknown, Reason: "the selected build version is not established offline and may lie inside the affected range"}
		default:
			return &Finding{State: StateNotAffected, Reason: fmt.Sprintf("declared minimum %s is at or above every fixed boundary, so the selected version cannot be inside the ranges", mod.Version)}
		}
	case ConfidenceObserved:
		switch {
		case explicitHit || len(in) > 0:
			return &Finding{State: StatePotentiallyAffected, Reason: fmt.Sprintf("an observed go.sum version lies inside the affected range; the selected version is not proven")}
		case len(uncertain) > 0:
			return &Finding{State: StateUnknown, Reason: "observed versions cannot be compared against every affected range"}
		default:
			return &Finding{State: StateNotAffected, Reason: "every version observed in go.sum lies outside the affected ranges"}
		}
	case ConfidenceRequirement:
		switch {
		case len(in) > 0 || explicitHit:
			return &Finding{State: StatePotentiallyAffected, Reason: fmt.Sprintf("requirement %s overlaps the affected range; the selected version is unknown", mod.Requirement)}
		case len(uncertain) > 0 || len(below) > 0:
			return &Finding{State: StateUnknown, Reason: "the requirement cannot be evaluated against every affected range"}
		default:
			return &Finding{State: StateNotAffected, Reason: fmt.Sprintf("requirement %s is disjoint from every affected range, so no version it admits can be affected", mod.Requirement)}
		}
	default:
		return nil
	}
}

// rankVerdict orders verdicts for multi-candidate reduction: membership
// wins over uncertainty, uncertainty over exclusion.
func rankVerdict(v rangeVerdict) int {
	switch v {
	case verdictIn:
		return 0
	case verdictBeyondLimit, verdictUnevaluable:
		return 1
	case verdictOutAbove, verdictOutBelow:
		return 2
	}
	return 3
}

// mergeDrafts collapses findings that share an alias for the same package:
// duplicate OSV records (for example a GHSA-based and an ecosystem-native
// record) become one finding with unioned provenance. The most concerning
// disposition wins; the ecosystem-native record is the primary identity.
func mergeDrafts(drafts []Finding) []Finding {
	if len(drafts) <= 1 {
		return drafts
	}
	parent := make([]int, len(drafts))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	aliasOwner := map[string]int{}
	for i, draft := range drafts {
		for _, alias := range append(append([]string{}, draft.Aliases...), draft.AdvisoryID) {
			if prev, ok := aliasOwner[alias]; ok {
				union(prev, i)
			} else {
				aliasOwner[alias] = i
			}
		}
	}

	groups := map[int][]Finding{}
	for i, draft := range drafts {
		root := find(i)
		groups[root] = append(groups[root], draft)
	}
	out := make([]Finding, 0, len(groups))
	for _, group := range groups {
		out = append(out, mergeGroup(group))
	}
	return out
}

func mergeGroup(group []Finding) Finding {
	if len(group) == 1 {
		return group[0]
	}
	primary := group[0]
	for _, draft := range group[1:] {
		if nativeIDRank(draft.AdvisoryID) < nativeIDRank(primary.AdvisoryID) {
			primary = draft
		} else if nativeIDRank(draft.AdvisoryID) == nativeIDRank(primary.AdvisoryID) && draft.AdvisoryID < primary.AdvisoryID {
			primary = draft
		}
	}
	merged := primary
	stateRank := map[string]int{
		StateAffected:            0,
		StatePotentiallyAffected: 1,
		StateUnknown:             2,
		StateNotAffected:         3,
	}
	for _, draft := range group {
		if draft.AdvisoryID == primary.AdvisoryID {
			continue
		}
		merged.Aliases = appendUnique(merged.Aliases, draft.AdvisoryID)
		merged.Aliases = appendMissing(merged.Aliases, draft.Aliases)
		merged.Related = appendMissing(merged.Related, draft.Related)
		merged.CVSSVectors = appendMissing(merged.CVSSVectors, draft.CVSSVectors)
		merged.References = appendMissing(merged.References, draft.References)
		merged.Ranges = appendMissing(merged.Ranges, draft.Ranges)
		if stateRank[draft.State] < stateRank[merged.State] {
			merged.State, merged.Reason, merged.FixedVersion = draft.State, draft.Reason, draft.FixedVersion
		}
		if draft.FixedVersion != "" && (merged.FixedVersion == "" || draft.FixedVersion > merged.FixedVersion) {
			merged.FixedVersion = draft.FixedVersion
		}
		if SeverityRank(draft.Severity) > SeverityRank(merged.Severity) {
			merged.Severity, merged.SeveritySource = draft.Severity, draft.SeveritySource
		}
		if draft.RetrievedAt.After(merged.RetrievedAt) {
			merged.RetrievedAt = draft.RetrievedAt
		}
	}
	merged.Aliases = sortStrings(merged.Aliases)
	merged.CVSSVectors = sortStrings(merged.CVSSVectors)
	return merged
}

// nativeIDRank prefers ecosystem-native advisory ids as primary identity.
func nativeIDRank(id string) int {
	switch {
	case strings.HasPrefix(id, "GO-"), strings.HasPrefix(id, "RUSTSEC-"):
		return 0
	case strings.HasPrefix(id, "OSV-"):
		return 1
	default:
		return 2
	}
}

func appendMissing(dst, src []string) []string {
	for _, v := range src {
		dst = appendUnique(dst, v)
	}
	return dst
}

func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Package != findings[j].Package {
			return findings[i].Package < findings[j].Package
		}
		if findings[i].State != findings[j].State {
			stateRank := map[string]int{StateAffected: 0, StatePotentiallyAffected: 1, StateUnknown: 2, StateNotAffected: 3}
			return stateRank[findings[i].State] < stateRank[findings[j].State]
		}
		if SeverityRank(findings[i].Severity) != SeverityRank(findings[j].Severity) {
			return SeverityRank(findings[i].Severity) > SeverityRank(findings[j].Severity)
		}
		return findings[i].AdvisoryID < findings[j].AdvisoryID
	})
}

// advisorySeverity maps OSV severity artifacts to the label vocabulary.
// A missing label stays unknown; CVSS vectors are preserved raw and never
// converted into a locally computed score.
func advisorySeverity(adv *OSVAdvisory, eco Ecosystem, name string) (string, string, []string) {
	var vectors []string
	for _, severity := range adv.Severity {
		if (severity.Type == "CVSS_V3" || severity.Type == "CVSS_V4") && severity.Score != "" {
			vectors = append(vectors, severity.Score)
		}
	}
	label, source := "", ""
	if raw := databaseSpecificSeverity(adv.DatabaseSpecific); raw != "" {
		label, source = normalizeSeverity(raw), "osv database_specific"
	}
	for _, affected := range adv.Affected {
		if !affectedMatches(affected, eco, name) {
			continue
		}
		if raw := databaseSpecificSeverity(affected.DatabaseSpecific); raw != "" && label == "" {
			label, source = normalizeSeverity(raw), "osv database_specific"
		}
	}
	if label == "" {
		label = SeverityUnknown
	}
	return label, source, sortStrings(vectors)
}

func databaseSpecificSeverity(specific map[string]any) string {
	if specific == nil {
		return ""
	}
	if raw, ok := specific["severity"].(string); ok {
		return raw
	}
	return ""
}

func normalizeSeverity(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "LOW":
		return SeverityLow
	case "MODERATE", "MEDIUM":
		return SeverityMedium
	case "HIGH":
		return SeverityHigh
	case "CRITICAL":
		return SeverityCritical
	default:
		return SeverityUnknown
	}
}

func advisorySummary(adv *OSVAdvisory) string {
	summary := strings.TrimSpace(adv.Summary)
	const maxSummary = 300
	if len(summary) > maxSummary {
		summary = summary[:maxSummary]
	}
	return summary
}

// advisoryReferences keeps a bounded, sanitized reference list.
func advisoryReferences(adv *OSVAdvisory) []string {
	const maxReferences = 8
	var out []string
	for _, ref := range adv.References {
		if ref.URL == "" || !strings.HasPrefix(ref.URL, "http") {
			continue
		}
		out = append(out, ref.URL)
		if len(out) >= maxReferences {
			break
		}
	}
	return out
}
