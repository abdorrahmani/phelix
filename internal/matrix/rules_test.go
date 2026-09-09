package matrix

import (
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

func TestRuleFromFields_EcosystemShorthand(t *testing.T) {
	rule := RuleFromFields(map[string]string{"go": "1.25", "platform": "windows/amd64"})
	if rule.Dimensions[DimLang] != "go" || rule.Dimensions[DimVersion] != "1.25" {
		t.Fatalf("shorthand not resolved: %+v", rule.Dimensions)
	}
	if rule.Dimensions[DimOS] != "windows" || rule.Dimensions[DimArch] != "amd64" {
		t.Fatalf("platform not expanded: %+v", rule.Dimensions)
	}
}

func TestRuleFromFields_NormalizesValues(t *testing.T) {
	rule := RuleFromFields(map[string]string{"rust": "rust1.77", "platform": "LINUX/ARM/V7"})
	if rule.Dimensions[DimVersion] != "1.77" {
		t.Fatalf("version not normalized: %q", rule.Dimensions[DimVersion])
	}
	if rule.Dimensions[DimVariant] != "v7" || rule.Dimensions[DimOS] != "linux" {
		t.Fatalf("platform not normalized: %+v", rule.Dimensions)
	}
}

func TestRuleFromFields_UnknownKeysBecomeMetadata(t *testing.T) {
	rule := RuleFromFields(map[string]string{"go": "1.27", "platform": "linux/amd64", "tag": "latest"})
	if rule.Metadata["tag"] != "latest" {
		t.Fatalf("metadata lost: %+v", rule.Metadata)
	}
	if _, isDim := rule.Dimensions["tag"]; isDim {
		t.Fatal("metadata leaked into dimensions")
	}
}

func TestRuleFromFields_MalformedPlatformKeptForValidation(t *testing.T) {
	rule := RuleFromFields(map[string]string{"platform": "not-a-platform"})
	if rule.Dimensions["platform"] != "not-a-platform" {
		t.Fatalf("malformed platform silently dropped: %+v", rule.Dimensions)
	}
	if err := ValidateRule("exclude", rule); err == nil {
		t.Fatal("malformed platform accepted")
	}
}

func TestValidateRule_RejectsEmptyAndUnknown(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		rule  Rule
		field string
	}{
		{"empty exclude rule", "exclude", Rule{}, ""},
		{"empty include rule", "include", Rule{}, ""},
		{"exclude with unknown key", "exclude",
			RuleFromFields(map[string]string{"go": "1.25", "tag": "x"}), "tag"},
		{"empty dimension value", "exclude",
			RuleFromFields(map[string]string{"version": " "}), ""},
		{"bad lang", "include",
			Rule{Dimensions: Dimensions{DimLang: "python", DimVersion: "3.12", DimOS: "linux", DimArch: "amd64"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRule(tc.kind, tc.rule)
			if err == nil {
				t.Fatal("invalid rule accepted")
			}
			if phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
				t.Fatalf("code = %s", phelixerr.CodeOf(err))
			}
			if tc.field != "" && !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error %q does not name the offending key %q", err, tc.field)
			}
		})
	}
}

func TestValidateRule_AcceptsValidRules(t *testing.T) {
	if err := ValidateRule("include", RuleFromFields(map[string]string{"go": "1.27", "platform": "linux/amd64", "tag": "latest"})); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRule("exclude", RuleFromFields(map[string]string{"go": "1.25", "platform": "windows/amd64"})); err != nil {
		t.Fatal(err)
	}
}

func TestRuleMatches_PartialMatching(t *testing.T) {
	rule := RuleFromFields(map[string]string{"go": "1.25", "platform": "windows/amd64"})
	match := Combination{Lang: builder.Go, Version: "1.25", OS: "windows", Arch: "amd64", Platform: "windows/amd64"}
	otherVersion := Combination{Lang: builder.Go, Version: "1.26", OS: "windows", Arch: "amd64", Platform: "windows/amd64"}
	otherPlatform := Combination{Lang: builder.Go, Version: "1.25", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}

	if !rule.Matches(match) {
		t.Fatal("exact match failed")
	}
	if rule.Matches(otherVersion) || rule.Matches(otherPlatform) {
		t.Fatal("partial rule matched a non-matching combination")
	}
}

func TestRuleMatches_VersionOnlyMatchesAllPlatforms(t *testing.T) {
	rule := RuleFromFields(map[string]string{"go": "1.26"})
	for _, plat := range []string{"linux/amd64", "linux/arm64", "windows/amd64"} {
		c := Combination{Lang: builder.Go, Version: "1.26", OS: strings.SplitN(plat, "/", 2)[0], Arch: "amd64", Platform: plat}
		if !rule.Matches(c) {
			t.Fatalf("version-only rule must match %s", plat)
		}
	}
	c := Combination{Lang: builder.Go, Version: "1.27", OS: "linux", Arch: "amd64", Platform: "linux/amd64"}
	if rule.Matches(c) {
		t.Fatal("version-only rule matched a different version")
	}
}

func TestRuleDescribe_HumanReadable(t *testing.T) {
	rule := RuleFromFields(map[string]string{"go": "1.25", "platform": "windows/amd64"})
	d := rule.Describe()
	if !strings.Contains(d, "go=1.25") || !strings.Contains(d, "os=windows") || !strings.Contains(d, "arch=amd64") {
		t.Fatalf("describe = %q", d)
	}
}
