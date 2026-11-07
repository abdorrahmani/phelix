package security

import (
	"fmt"
	"strings"
)

// cargoVer is a parsed Rust crate version following the semver 2.0.0
// ordering Cargo uses. Build metadata is preserved for display but never
// participates in comparison.
type cargoVer struct {
	major, minor, patch uint64
	pre                 []cargoPreID
}

type cargoPreID struct {
	num   uint64
	text  string
	isNum bool
}

// parseCargoVersion parses a crate version such as 1.2.3, 1.2.3-rc.1, or
// 1.2.3+build.5. A leading v is tolerated. Pre-release forms require all
// three numeric components, as semver demands.
func parseCargoVersion(s string) (cargoVer, bool) {
	var v cargoVer
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return v, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core, s = s[:i], s[i+1:]
	}
	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return v, false
	}
	if s != core && len(parts) != 3 {
		return v, false
	}
	nums := [3]uint64{}
	for i := 0; i < len(parts); i++ {
		if !allDigits(parts[i]) {
			return v, false
		}
		n, ok := parseUint(parts[i])
		if !ok {
			return v, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	if s != core {
		for _, p := range strings.Split(s, ".") {
			if p == "" {
				return v, false
			}
			if allDigits(p) {
				n, ok := parseUint(p)
				if !ok {
					return v, false
				}
				v.pre = append(v.pre, cargoPreID{num: n, isNum: true})
			} else {
				v.pre = append(v.pre, cargoPreID{text: p})
			}
		}
	}
	return v, true
}

// compareCargoVersions returns -1, 0, or 1 for two crate versions
// following semver 2.0.0: numeric pre-release identifiers compare
// numerically, alphanumeric lexically, numeric sorts below alphanumeric,
// and a version without pre-release outranks one with it.
func compareCargoVersions(a, b cargoVer) int {
	if a.major != b.major {
		return uintCompare(a.major, b.major)
	}
	if a.minor != b.minor {
		return uintCompare(a.minor, b.minor)
	}
	if a.patch != b.patch {
		return uintCompare(a.patch, b.patch)
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		switch {
		case x.isNum && y.isNum:
			if x.num != y.num {
				return uintCompare(x.num, y.num)
			}
		case x.isNum:
			return -1
		case y.isNum:
			return 1
		default:
			if x.text != y.text {
				return strings.Compare(x.text, y.text)
			}
		}
	}
	return uintCompare(uint64(len(a.pre)), uint64(len(b.pre)))
}

// CompareCargoVersionStrings compares two crate version strings. It fails
// when either string is not a valid crate version; callers must treat
// that as an unevaluable version, never as an ordering.
func CompareCargoVersionStrings(a, b string) (int, error) {
	va, oka := parseCargoVersion(a)
	vb, okb := parseCargoVersion(b)
	if !oka || !okb {
		return 0, fmt.Errorf("cannot compare crate versions %q and %q", a, b)
	}
	return compareCargoVersions(va, vb), nil
}

func uintCompare(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseUint(s string) (uint64, bool) {
	if !allDigits(s) {
		return 0, false
	}
	var n uint64
	for _, r := range s {
		n = n*10 + uint64(r-'0')
	}
	return n, true
}

// cargoInterval is the set of crate versions a manifest requirement or an
// advisory range admits, with explicit inclusivity and open bounds.
type cargoInterval struct {
	lo, hi         cargoVer
	loInc, hiInc   bool
	unboundedAbove bool
	unboundedBelow bool
	empty          bool
}

// parseCargoRequirement compiles a Cargo version requirement into the
// interval of versions it admits. Supported syntax: bare versions
// (caret), ^, ~, =, >, >=, <, <=, comma-separated comparators, and
// wildcard tails such as 1.* or 1.2.*. Requirements containing
// pre-release identifiers are rejected: Cargo matches them by special
// pre-release rules this conservative parser does not approximate, and
// callers must treat the rejection as unevaluable evidence.
func parseCargoRequirement(req string) (cargoInterval, error) {
	req = strings.TrimSpace(req)
	if req == "" || req == "*" {
		return cargoInterval{unboundedAbove: true, unboundedBelow: true}, nil
	}

	iv := cargoInterval{unboundedAbove: true, unboundedBelow: true}
	for _, part := range strings.Split(req, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bound, err := cargoReqBound(part)
		if err != nil {
			return cargoInterval{}, err
		}
		iv = cargoIntersect(iv, bound)
		if iv.empty {
			return iv, nil
		}
	}
	return iv, nil
}

// cargoReqBound compiles one comparator into its interval.
func cargoReqBound(part string) (cargoInterval, error) {
	var op string
	for _, candidate := range []string{">=", "<=", "^", "~", "=", ">", "<"} {
		if strings.HasPrefix(part, candidate) {
			op = candidate
			part = strings.TrimSpace(strings.TrimPrefix(part, candidate))
			break
		}
	}

	wildcard := false
	if strings.HasSuffix(part, ".*") {
		wildcard = true
		part = strings.TrimSuffix(part, ".*")
	} else if part == "*" {
		return cargoInterval{unboundedAbove: true, unboundedBelow: true}, nil
	}
	if strings.Contains(part, "*") {
		return cargoInterval{}, fmt.Errorf("unsupported wildcard placement in requirement %q", part)
	}
	componentCount := len(strings.Split(part, "."))

	v, ok := parseCargoVersion(part)
	if !ok {
		return cargoInterval{}, fmt.Errorf("unparsable version requirement %q", part)
	}
	if len(v.pre) > 0 {
		return cargoInterval{}, fmt.Errorf("requirement %q contains a pre-release version; Cargo matches it by special rules", part)
	}

	switch {
	case wildcard:
		return cargoWildcardInterval(v, componentCount), nil
	case op == "" || op == "^":
		return cargoCaretInterval(v), nil
	case op == "~":
		return cargoTildeInterval(v, componentCount), nil
	case op == "=":
		return cargoInterval{lo: v, hi: v, loInc: true, hiInc: true}, nil
	case op == ">":
		return cargoInterval{lo: v, unboundedAbove: true}, nil
	case op == ">=":
		return cargoInterval{lo: v, loInc: true, unboundedAbove: true}, nil
	case op == "<":
		return cargoInterval{hi: v, unboundedBelow: true}, nil
	case op == "<=":
		return cargoInterval{hi: v, hiInc: true, unboundedBelow: true}, nil
	}
	return cargoInterval{}, fmt.Errorf("unsupported requirement operator in %q", part)
}

// cargoCaretInterval implements caret semantics: the next breaking bump
// bounds the interval from above.
func cargoCaretInterval(v cargoVer) cargoInterval {
	upper := v
	switch {
	case v.major > 0:
		upper = cargoVer{major: v.major + 1}
	case v.minor > 0:
		upper = cargoVer{minor: v.minor + 1}
	default:
		upper = cargoVer{patch: v.patch + 1}
	}
	return cargoInterval{lo: v, loInc: true, hi: upper}
}

// cargoTildeInterval implements tilde semantics: the patch level may move,
// or the minor level when no patch component was given.
func cargoTildeInterval(v cargoVer, componentCount int) cargoInterval {
	if componentCount <= 1 {
		return cargoInterval{lo: v, loInc: true, hi: cargoVer{major: v.major + 1}}
	}
	return cargoInterval{lo: v, loInc: true, hi: cargoVer{major: v.major, minor: v.minor + 1}}
}

// cargoWildcardInterval implements X.* and X.Y.* wildcards.
func cargoWildcardInterval(v cargoVer, componentCount int) cargoInterval {
	if componentCount <= 1 {
		return cargoInterval{lo: cargoVer{major: v.major}, loInc: true, hi: cargoVer{major: v.major + 1}}
	}
	return cargoInterval{lo: cargoVer{major: v.major, minor: v.minor}, loInc: true, hi: cargoVer{major: v.major, minor: v.minor + 1}}
}

// cargoIntersect narrows a by b, tracking whether the intersection is
// empty and which bounds stay open.
func cargoIntersect(a, b cargoInterval) cargoInterval {
	if a.empty || b.empty {
		return cargoInterval{empty: true}
	}
	out := a
	if !b.unboundedBelow {
		cmp := cargoIntervalCompareLower(a, b)
		if cmp < 0 || (cmp == 0 && !b.loInc && a.loInc) {
			out.lo, out.loInc, out.unboundedBelow = b.lo, b.loInc, false
		}
	}
	if !b.unboundedAbove {
		cmp := cargoIntervalCompareUpper(a, b)
		if cmp > 0 || (cmp == 0 && !b.hiInc && a.hiInc) {
			out.hi, out.hiInc, out.unboundedAbove = b.hi, b.hiInc, false
		}
	}
	if !out.unboundedBelow && !out.unboundedAbove {
		cmp := compareCargoVersions(out.lo, out.hi)
		if cmp > 0 || (cmp == 0 && !(out.loInc && out.hiInc)) {
			return cargoInterval{empty: true}
		}
	}
	return out
}

func cargoIntervalCompareLower(a, b cargoInterval) int {
	if a.unboundedBelow {
		return -1
	}
	if b.unboundedBelow {
		return 1
	}
	return compareCargoVersions(a.lo, b.lo)
}

func cargoIntervalCompareUpper(a, b cargoInterval) int {
	if a.unboundedAbove {
		return 1
	}
	if b.unboundedAbove {
		return -1
	}
	return compareCargoVersions(a.hi, b.hi)
}

// cargoVersionInInterval reports whether v lies inside iv. An unparsable
// version is never inside an interval; callers must treat unparsable
// versions as unevaluable, not as out of range.
func cargoVersionInInterval(v cargoVer, iv cargoInterval) bool {
	if iv.empty {
		return false
	}
	if !iv.unboundedBelow {
		cmp := compareCargoVersions(v, iv.lo)
		if cmp < 0 || (cmp == 0 && !iv.loInc) {
			return false
		}
	}
	if !iv.unboundedAbove {
		cmp := compareCargoVersions(v, iv.hi)
		if cmp > 0 || (cmp == 0 && !iv.hiInc) {
			return false
		}
	}
	return true
}

// cargoOverlaps reports whether an advisory range and a manifest
// requirement admit at least one common version.
func cargoOverlaps(rangeIv, req cargoInterval) bool {
	return !cargoIntersect(rangeIv, req).empty
}
