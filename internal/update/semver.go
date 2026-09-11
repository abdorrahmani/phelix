package update

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// Semver is a parsed Phelix release version: a numeric X.Y.Z triple with an
// optional prerelease suffix.
type Semver struct {
	Major, Minor, Patch uint64
	Pre                 string
}

// semverRe accepts the version shapes Phelix releases in practice: "1.2.3",
// "v1.2.3", "1.2.3-rc.1", "1.2.3+build.5", "1.2.3-rc.1+build.5", and the
// lenient "1.2.3dev" form used by early release tooling (its trailing
// letters are treated as a prerelease). Anything without a numeric X.Y.Z
// prefix — or with a trailing segment that cannot be a prerelease, like
// "1.2.3.4" or "1.2.3-" — is rejected. Build metadata is captured but
// ignored for comparison, per semver.
var semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)` +
	`(?:-?([0-9A-Za-z][0-9A-Za-z-]*(?:\.[0-9A-Za-z-]+)*))?` +
	`(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

// ParseSemver parses a version string, tolerating the "v"/"V" prefix and a
// missing prerelease separator ("1.2.3dev"). Malformed input yields a
// structured CodeConfiguration error.
func ParseSemver(s string) (Semver, error) {
	normalized := strings.TrimSpace(s)
	normalized = strings.TrimPrefix(normalized, "v")
	normalized = strings.TrimPrefix(normalized, "V")
	m := semverRe.FindStringSubmatch(normalized)
	if m == nil {
		return Semver{}, phelixerr.Newf(phelixerr.CodeConfiguration, "malformed version %q", s)
	}
	major, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return Semver{}, phelixerr.Newf(phelixerr.CodeConfiguration, "malformed version %q", s)
	}
	minor, err := strconv.ParseUint(m[2], 10, 64)
	if err != nil {
		return Semver{}, phelixerr.Newf(phelixerr.CodeConfiguration, "malformed version %q", s)
	}
	patch, err := strconv.ParseUint(m[3], 10, 64)
	if err != nil {
		return Semver{}, phelixerr.Newf(phelixerr.CodeConfiguration, "malformed version %q", s)
	}
	return Semver{Major: major, Minor: minor, Patch: patch, Pre: m[4]}, nil
}

// Compare orders two versions following semver rules: numeric triples first,
// then prereleases (a release sorts above its own prereleases).
func Compare(a, b Semver) int {
	switch {
	case a.Major != b.Major:
		return cmpUint(a.Major, b.Major)
	case a.Minor != b.Minor:
		return cmpUint(a.Minor, b.Minor)
	case a.Patch != b.Patch:
		return cmpUint(a.Patch, b.Patch)
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	return comparePre(a.Pre, b.Pre)
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// comparePre compares dot-separated prerelease identifiers: numeric
// identifiers sort numerically and below alphanumeric ones; otherwise
// ASCII-lexicographically.
func comparePre(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.ParseUint(as[i], 10, 64)
		bn, berr := strconv.ParseUint(bs[i], 10, 64)
		switch {
		case aerr == nil && berr == nil:
			if c := cmpUint(an, bn); c != 0 {
				return c
			}
		case aerr == nil:
			return -1
		case berr == nil:
			return 1
		default:
			if as[i] != bs[i] {
				if as[i] < bs[i] {
					return -1
				}
				return 1
			}
		}
	}
	return cmpInt(len(as), len(bs))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Display renders v with the canonical "v" prefix, e.g. "v1.2.3" or
// "v1.2.3-rc.1".
func Display(v Semver) string {
	if v.Pre == "" {
		return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	return fmt.Sprintf("v%d.%d.%d-%s", v.Major, v.Minor, v.Patch, v.Pre)
}

// isUnknownVersion reports whether s is the unversioned development default
// shipped by the ldflags-free build. Such builds carry no release semantics,
// so the updater refuses to guess whether a release is newer.
func isUnknownVersion(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || s == "0.0.0-dev"
}
