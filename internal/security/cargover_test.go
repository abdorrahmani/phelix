package security

import (
	"strings"
	"testing"
)

func TestCompareCargoVersionStrings(t *testing.T) {
	ordered := []string{
		"0.9.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0",
	}
	for i := 0; i < len(ordered)-1; i++ {
		cmp, err := CompareCargoVersionStrings(ordered[i], ordered[i+1])
		if err != nil || cmp >= 0 {
			t.Fatalf("%s must order below %s: cmp=%d err=%v", ordered[i], ordered[i+1], cmp, err)
		}
		cmp, err = CompareCargoVersionStrings(ordered[i+1], ordered[i])
		if err != nil || cmp <= 0 {
			t.Fatalf("%s must order above %s: cmp=%d err=%v", ordered[i+1], ordered[i], cmp, err)
		}
	}
	cmp, err := CompareCargoVersionStrings("1.0.0+build.5", "1.0.0+other")
	if err != nil || cmp != 0 {
		t.Fatalf("build metadata must not affect ordering: cmp=%d err=%v", cmp, err)
	}
	if _, err := CompareCargoVersionStrings("not-a-version", "1.0.0"); err == nil {
		t.Fatal("unparsable versions must fail, not order")
	}
}

func TestParseCargoVersion_Rejects(t *testing.T) {
	for _, bad := range []string{"", "1.2.3-", "1.2-rc", "1.2.x", "abc", "1.2.3.4"} {
		if _, ok := parseCargoVersion(bad); ok {
			t.Fatalf("%q must not parse as a crate version", bad)
		}
	}
	if v, ok := parseCargoVersion("1.2"); !ok || v.major != 1 || v.minor != 2 || v.patch != 0 {
		t.Fatalf("two-component versions parse with a zero patch: %+v %v", v, ok)
	}
}

func TestParseCargoRequirement(t *testing.T) {
	cases := []struct {
		req      string
		in       []string
		out      []string
		mustFail bool
	}{
		{req: "1.2.3", in: []string{"1.2.3", "1.9.9"}, out: []string{"2.0.0", "1.2.2"}},
		{req: "^0.2.3", in: []string{"0.2.3", "0.2.9"}, out: []string{"0.3.0", "1.0.0"}},
		{req: "^0.0.3", in: []string{"0.0.3"}, out: []string{"0.0.4"}},
		{req: "~1.2.3", in: []string{"1.2.3", "1.2.9"}, out: []string{"1.3.0"}},
		{req: "~1.2", in: []string{"1.2.0", "1.2.9"}, out: []string{"1.3.0"}},
		{req: ">=1.2, <1.6", in: []string{"1.2.0", "1.5.9"}, out: []string{"1.6.0", "1.1.9"}},
		{req: "=1.2.3", in: []string{"1.2.3"}, out: []string{"1.2.4", "1.2.2"}},
		{req: "1.*", in: []string{"1.0.0", "1.9.9"}, out: []string{"2.0.0", "0.9.9"}},
		{req: "1.2.*", in: []string{"1.2.0", "1.2.99"}, out: []string{"1.3.0", "1.1.0"}},
		{req: "*", in: []string{"0.0.1", "9.9.9"}, out: nil},
		{req: ">2.0", in: []string{"2.0.1", "3.0.0"}, out: []string{"2.0.0"}},
		{req: "<=1.0", in: []string{"1.0.0", "0.9.0"}, out: []string{"1.0.1"}},
		{req: "1.0.0-rc.1", mustFail: true},
		{req: ">=2.0, <1.0", mustFail: false, in: nil, out: []string{"1.5.0", "2.5.0"}},
	}
	for _, tc := range cases {
		iv, err := parseCargoRequirement(tc.req)
		if tc.mustFail {
			if err == nil {
				t.Fatalf("requirement %q must fail to parse", tc.req)
			}
			continue
		}
		if err != nil {
			t.Fatalf("requirement %q: %v", tc.req, err)
		}
		for _, v := range tc.in {
			parsed, ok := parseCargoVersion(v)
			if !ok || !cargoVersionInInterval(parsed, iv) {
				t.Fatalf("requirement %q must admit %s", tc.req, v)
			}
		}
		for _, v := range tc.out {
			parsed, ok := parseCargoVersion(v)
			if !ok {
				t.Fatalf("test bug: %q does not parse", v)
				continue
			}
			if cargoVersionInInterval(parsed, iv) {
				t.Fatalf("requirement %q must reject %s", tc.req, v)
			}
		}
	}
}

func TestCargoOverlaps(t *testing.T) {
	cases := []struct {
		name  string
		rng   string
		req   string
		match bool
	}{
		{"range inside requirement", "1.0.0 2.0.0", "1.5", true},
		{"range below requirement", "1.0.0 2.0.0", "3.0", false},
		{"range above requirement", "3.0.0 4.0.0", "1.5", false},
		{"open-ended range above requirement floor", "1.0.0 -", "0.9", false},
		{"open-ended range meets requirement", "1.0.0 -", "1.5", true},
		{"range capped above requirement", "0.5.0 1.0.0", "1.0", false},
		{"exact version at range start", "1.0.0 2.0.0", "=1.0.0", true},
	}
	for _, tc := range cases {
		rng, err := cargoTestRange(tc.rng)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		req, err := parseCargoRequirement(tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := cargoOverlaps(rng, req); got != tc.match {
			t.Fatalf("%s: overlap=%v want %v", tc.name, got, tc.match)
		}
	}
}

// cargoTestRange builds an interval from "lo hi" where "-" marks an open
// upper bound, mirroring an advisory range.
func cargoTestRange(spec string) (cargoInterval, error) {
	lo, hi := "", ""
	parts := strings.Fields(spec)
	if len(parts) != 2 {
		return cargoInterval{}, nil
	}
	lo, hi = parts[0], parts[1]
	if hi == "-" {
		lv, ok := parseCargoVersion(lo)
		if !ok {
			return cargoInterval{}, nil
		}
		return cargoInterval{lo: lv, loInc: true, unboundedAbove: true}, nil
	}
	lv, ok1 := parseCargoVersion(lo)
	hv, ok2 := parseCargoVersion(hi)
	if !ok1 || !ok2 {
		return cargoInterval{}, nil
	}
	return cargoInterval{lo: lv, loInc: true, hi: hv}, nil
}
