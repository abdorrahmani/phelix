package update

import (
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		input   string
		want    Semver
		wantErr bool
	}{
		{"1.2.3", Semver{1, 2, 3, ""}, false},
		{"v1.2.3", Semver{1, 2, 3, ""}, false},
		{"V1.2.3", Semver{1, 2, 3, ""}, false},
		{"  1.2.3  ", Semver{1, 2, 3, ""}, false},
		{"1.2.3-rc.1", Semver{1, 2, 3, "rc.1"}, false},
		// Build metadata is captured separately and ignored for comparison.
		{"1.2.3-rc.1+build.5", Semver{1, 2, 3, "rc.1"}, false},
		{"1.2.3+build.5", Semver{1, 2, 3, ""}, false},
		{"1.2.3dev", Semver{1, 2, 3, "dev"}, false},
		{"0.0.1dev", Semver{0, 0, 1, "dev"}, false},
		{"0.0.0-dev", Semver{0, 0, 0, "dev"}, false},
		{"1.2", Semver{}, true},
		{"1", Semver{}, true},
		{"", Semver{}, true},
		{"v", Semver{}, true},
		{"abc", Semver{}, true},
		{"1.2.3.4", Semver{}, true},
		{"1.2.3-", Semver{}, true},
		{"1.2.3+", Semver{}, true},
		{"version-1.2.3", Semver{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseSemver(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSemver(%q) expected error, got %+v", tc.input, got)
				}
				if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
					t.Fatalf("ParseSemver(%q) error = %v, want CodeConfiguration", tc.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSemver(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSemver(%q) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0}, // prefix normalized away
		{"1.2.3", "1.2.4", -1},
		{"1.2.4", "1.2.3", 1},
		{"1.9.9", "2.0.0", -1},
		{"2.0.0", "1.9.9", 1},
		{"1.2.3", "1.3.0", -1},
		{"0.0.1dev", "0.0.1", -1}, // prerelease sorts below release
		{"0.0.1", "0.0.1dev", 1},
		{"1.2.3-rc.1", "1.2.3-rc.2", -1},
		{"1.2.3-rc.2", "1.2.3-rc.10", -1}, // numeric identifier comparison
		{"1.2.3-alpha", "1.2.3-beta", -1},
		{"1.2.3-1", "1.2.3-alpha", -1}, // numeric < alphanumeric
		{"1.2.3-rc", "1.2.3-rc.1", -1}, // fewer identifiers sort lower
		{"1.2.3-rc.1", "1.2.3", -1},
		{"1.2.3", "1.2.3-rc.1", 1},
	}
	for _, tc := range cases {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			a, err := ParseSemver(tc.a)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.a, err)
			}
			b, err := ParseSemver(tc.b)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.b, err)
			}
			if got := Compare(a, b); got != tc.want {
				t.Fatalf("Compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestDisplay(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"1.2.3", "v1.2.3"},
		{"v1.2.3", "v1.2.3"},
		{"1.2.3-rc.1", "v1.2.3-rc.1"},
		{"0.0.1dev", "v0.0.1-dev"},
	}
	for _, tc := range cases {
		v, err := ParseSemver(tc.in)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.in, err)
		}
		if got := Display(v); got != tc.want {
			t.Fatalf("Display(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsUnknownVersion(t *testing.T) {
	for _, s := range []string{"", "  ", "0.0.0-dev"} {
		if !isUnknownVersion(s) {
			t.Fatalf("isUnknownVersion(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"1.2.3", "v1.2.3", "0.0.1dev", "0.0.0"} {
		if isUnknownVersion(s) {
			t.Fatalf("isUnknownVersion(%q) = true, want false", s)
		}
	}
}
