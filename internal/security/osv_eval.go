package security

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

// rangeVerdict is the disposition of one advisory range against version
// evidence. outBelow and outAbove are kept apart: for Go declared-minimum
// evidence, a candidate below an introduced boundary does not clear the
// advisory, because the selected version may be higher.
type rangeVerdict int

const (
	verdictOutAbove rangeVerdict = iota
	verdictIn
	verdictOutBelow
	verdictBeyondLimit
	verdictUnevaluable
)

// compareVersion compares two ecosystem version strings and fails on
// unparsable input. The value "0" is the OSV convention for "all versions
// from the beginning" and compares below everything.
type compareVersion func(a, b string) (int, error)

func goVersionCompare(a, b string) (int, error) {
	if a == "0" && b == "0" {
		return 0, nil
	}
	if a == "0" {
		return -1, nil
	}
	if b == "0" {
		return 1, nil
	}
	na, nb := goSemver(a), goSemver(b)
	if !semver.IsValid(na) || !semver.IsValid(nb) {
		return 0, fmt.Errorf("cannot compare Go module versions %q and %q", a, b)
	}
	return semver.Compare(na, nb), nil
}

func cargoVersionCompare(a, b string) (int, error) {
	if a == "0" && b == "0" {
		return 0, nil
	}
	if a == "0" {
		return -1, nil
	}
	if b == "0" {
		return 1, nil
	}
	return CompareCargoVersionStrings(a, b)
}

func ecosystemCompare(eco Ecosystem) compareVersion {
	if eco == EcosystemGo {
		return goVersionCompare
	}
	return cargoVersionCompare
}

type osvSegment struct {
	introduced string
	// upper event: fixed, last_affected, or limit; empty means open.
	fixed        string
	lastAffected string
	limit        string
}

// parseOSVRangeEvents converts an event sequence into segments. Events are
// processed in the order the advisory lists them: introduced opens a
// segment, fixed, last_affected, or limit closes it.
func parseOSVRangeEvents(events []map[string]string) ([]osvSegment, bool) {
	var segments []osvSegment
	current := -1
	for _, event := range events {
		if value, ok := event["introduced"]; ok {
			segments = append(segments, osvSegment{introduced: value})
			current = len(segments) - 1
			continue
		}
		if current < 0 {
			continue
		}
		switch {
		case hasEventKey(event, "fixed"):
			segments[current].fixed = event["fixed"]
			current = -1
		case hasEventKey(event, "last_affected"):
			segments[current].lastAffected = event["last_affected"]
			current = -1
		case hasEventKey(event, "limit"):
			segments[current].limit = event["limit"]
			current = -1
		}
	}
	if len(segments) == 0 {
		return nil, false
	}
	return segments, true
}

func hasEventKey(event map[string]string, key string) bool {
	value, ok := event[key]
	return ok && value != ""
}

// evaluateOSVRange decides where one range places the candidate version.
// comparer must match the range's ecosystem semantics.
func evaluateOSVRange(rng OSVRange, candidate string, compare compareVersion) (rangeVerdict, string, error) {
	segments, ok := parseOSVRangeEvents(rng.Events)
	if !ok {
		return verdictUnevaluable, "", fmt.Errorf("range carries no introduced event")
	}

	verdicts := make([]rangeVerdict, 0, len(segments))
	for _, segment := range segments {
		verdict, err := evaluateSegment(segment, candidate, compare)
		if err != nil {
			return verdictUnevaluable, "", err
		}
		verdicts = append(verdicts, verdict)
	}

	final := verdicts[0]
	for _, v := range verdicts[1:] {
		final = combineVerdicts(final, v)
	}
	return final, osvRangeSummary(rng.Type, segments), nil
}

// evaluateSegment places the candidate against one introduced..upper
// segment.
func evaluateSegment(segment osvSegment, candidate string, compare compareVersion) (rangeVerdict, error) {
	afterIntroduced, err := compare(candidate, segment.introduced)
	if err != nil {
		return verdictUnevaluable, err
	}
	if afterIntroduced < 0 {
		return verdictOutBelow, nil
	}

	switch {
	case segment.fixed != "":
		cmp, err := compare(candidate, segment.fixed)
		if err != nil {
			return verdictUnevaluable, err
		}
		if cmp < 0 {
			return verdictIn, nil
		}
		return verdictOutAbove, nil
	case segment.lastAffected != "":
		cmp, err := compare(candidate, segment.lastAffected)
		if err != nil {
			return verdictUnevaluable, err
		}
		if cmp <= 0 {
			return verdictIn, nil
		}
		return verdictOutAbove, nil
	case segment.limit != "":
		cmp, err := compare(candidate, segment.limit)
		if err != nil {
			return verdictUnevaluable, err
		}
		if cmp < 0 {
			return verdictIn, nil
		}
		return verdictBeyondLimit, nil
	default:
		return verdictIn, nil
	}
}

// combineVerdicts merges the verdicts of multiple segments of one range:
// any confident membership wins, then uncertainty, then the confident
// exclusions.
func combineVerdicts(a, b rangeVerdict) rangeVerdict {
	rank := func(v rangeVerdict) int {
		switch v {
		case verdictIn:
			return 0
		case verdictBeyondLimit, verdictUnevaluable:
			return 1
		case verdictOutBelow, verdictOutAbove:
			return 2
		}
		return 3
	}
	if rank(a) <= rank(b) {
		return a
	}
	return b
}

// osvRangeSummary renders one range in a stable human-readable form.
func osvRangeSummary(kind string, segments []osvSegment) string {
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch {
		case segment.fixed != "":
			parts = append(parts, fmt.Sprintf("[%s, %s)", segment.introduced, segment.fixed))
		case segment.lastAffected != "":
			parts = append(parts, fmt.Sprintf("[%s, %s]", segment.introduced, segment.lastAffected))
		case segment.limit != "":
			parts = append(parts, fmt.Sprintf("[%s, %s)", segment.introduced, segment.limit))
		default:
			parts = append(parts, fmt.Sprintf("[%s, ...)", segment.introduced))
		}
	}
	if kind != "" {
		return kind + ":" + strings.Join(parts, " ")
	}
	return strings.Join(parts, " ")
}

// osvFixedVersion returns the highest fixed boundary across the advisory's
// affected entries for one package.
func osvFixedVersion(adv *OSVAdvisory, eco Ecosystem, name string) string {
	fixed := ""
	for _, affected := range adv.Affected {
		if !affectedMatches(affected, eco, name) {
			continue
		}
		for _, rng := range affected.Ranges {
			segments, ok := parseOSVRangeEvents(rng.Events)
			if !ok {
				continue
			}
			for _, segment := range segments {
				if segment.fixed == "" {
					continue
				}
				if fixed == "" {
					fixed = segment.fixed
					continue
				}
				compare := ecosystemCompare(eco)
				if cmp, cerr := compare(segment.fixed, fixed); cerr == nil && cmp > 0 {
					fixed = segment.fixed
				}
			}
		}
	}
	return fixed
}

// affectedMatches reports whether an affected entry describes the package.
func affectedMatches(affected OSVAffected, eco Ecosystem, name string) bool {
	want, err := osvEcosystemName(eco)
	if err != nil {
		return false
	}
	return affected.Package.Ecosystem == want && affected.Package.Name == name
}

// sortStrings is a tiny helper for stable alias output.
func sortStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
