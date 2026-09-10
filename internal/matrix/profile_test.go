package matrix

import (
	"reflect"
	"testing"
	"time"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// yamlProfile is a shorthand for building the normalized phelix.yaml profile.
func yamlProfile(lang builder.Language, versions, platforms []string, concurrency int) *Profile {
	return &Profile{Lang: lang, Versions: versions, Platforms: platforms, Concurrency: concurrency}
}

func comboIDs(plan *MatrixPlan) []string {
	ids := make([]string, 0, len(plan.Combinations))
	for _, c := range plan.Combinations {
		ids = append(ids, c.ID())
	}
	return ids
}

// --- Activity -----------------------------------------------------------------

func TestResolve_InactiveByDefault(t *testing.T) {
	prof, active, err := Resolve(ResolveInput{DetectedLang: builder.Go})
	if err != nil || active || prof != nil {
		t.Fatalf("expected inactive, got prof=%v active=%v err=%v", prof, active, err)
	}
}

func TestResolve_MatrixFlagActivates(t *testing.T) {
	prof, active, err := Resolve(ResolveInput{DetectedLang: builder.Go, MatrixFlag: true})
	if err != nil || !active {
		t.Fatalf("expected active, got active=%v err=%v", active, err)
	}
	if prof == nil || len(prof.Versions) != 0 {
		t.Fatalf("flag-only activation carries no versions; profile: %+v", prof)
	}
}

func TestResolve_ExplicitMatrixFalseDisables(t *testing.T) {
	in := ResolveInput{
		DetectedLang:  builder.Go,
		MatrixFlag:    false,
		MatrixFlagSet: true,
		YAML:          yamlProfile(builder.Go, []string{"1.26"}, []string{"linux/amd64"}, 0),
		YAMLEnabled:   true,
	}
	prof, active, err := Resolve(in)
	if err != nil || active || prof != nil {
		t.Fatalf("explicit --matrix=false must disable even an enabled profile; got prof=%v active=%v err=%v", prof, active, err)
	}
}

func TestResolve_ExplicitMatrixFalseWithDimensionFlagsRejected(t *testing.T) {
	// --matrix=false next to explicitly requested versions/platforms is
	// contradictory: the old behavior silently ignored the dimension flags and
	// ran a plain single build. It must fail fast instead.
	for _, tc := range []struct {
		name string
		cli  CLIOptions
	}{
		{"go versions", CLIOptions{GoVersions: []string{"1.26"}}},
		{"rust versions", CLIOptions{RustVersions: []string{"1.77"}}},
		{"platforms", CLIOptions{Platforms: []string{"linux/amd64"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := ResolveInput{
				DetectedLang:  builder.Go,
				MatrixFlag:    false,
				MatrixFlagSet: true,
				CLI:           tc.cli,
				YAMLEnabled:   true,
				YAML:          yamlProfile(builder.Go, []string{"1.26"}, []string{"linux/amd64"}, 0),
			}
			_, _, err := Resolve(in)
			if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
				t.Fatalf("expected invalid-argument error for --matrix=false + dimension flags, got %v", err)
			}
		})
	}
}

func TestResolve_YAMLProfileActivatesWithoutFlag(t *testing.T) {
	in := ResolveInput{
		DetectedLang: builder.Go,
		YAML:         yamlProfile(builder.Go, []string{"1.26", "1.27"}, []string{"linux/amd64"}, 0),
		YAMLEnabled:  true,
	}
	prof, active, err := Resolve(in)
	if err != nil || !active {
		t.Fatalf("expected active, err=%v", err)
	}
	if !reflect.DeepEqual(prof.Versions, []string{"1.26", "1.27"}) {
		t.Fatalf("expected YAML versions, got %v", prof.Versions)
	}
	if prof.Source.Versions != SourceConfig || prof.Source.Platforms != SourceConfig {
		t.Fatalf("expected phelix.yaml sources, got %+v", prof.Source)
	}
}

// --- Precedence: CLI > YAML > default ------------------------------------------

func TestResolve_CLIVersionsOverrideYAML(t *testing.T) {
	in := ResolveInput{
		DetectedLang: builder.Go,
		YAML:         yamlProfile(builder.Go, []string{"1.26", "1.27"}, []string{"linux/amd64", "linux/arm64"}, 2),
		YAMLEnabled:  true,
		CLI:          CLIOptions{GoVersions: []string{"1.28"}},
	}
	prof, _, err := Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	// CLI replaces the whole list — never merged with the YAML list.
	if !reflect.DeepEqual(prof.Versions, []string{"1.28"}) {
		t.Fatalf("CLI versions must replace YAML list, got %v", prof.Versions)
	}
	// Unmentioned dimensions keep their configured values.
	if !reflect.DeepEqual(prof.Platforms, []string{"linux/amd64", "linux/arm64"}) {
		t.Fatalf("YAML platforms must survive, got %v", prof.Platforms)
	}
	if prof.Concurrency != 2 || prof.Source.Concurrency != SourceConfig {
		t.Fatalf("YAML concurrency must survive, got %d (%s)", prof.Concurrency, prof.Source.Concurrency)
	}
	if prof.Source.Versions != SourceCLI {
		t.Fatalf("versions source = %s, want cli", prof.Source.Versions)
	}
}

func TestResolve_CLIPlatformsOverrideYAML(t *testing.T) {
	in := ResolveInput{
		DetectedLang: builder.Go,
		YAML:         yamlProfile(builder.Go, []string{"1.26"}, []string{"linux/amd64"}, 0),
		YAMLEnabled:  true,
		CLI:          CLIOptions{GoVersions: []string{"1.26"}, Platforms: []string{"darwin/arm64"}},
	}
	prof, _, err := Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prof.Platforms, []string{"darwin/arm64"}) {
		t.Fatalf("CLI platforms must replace YAML list, got %v", prof.Platforms)
	}
}

func TestResolve_ConcurrencyPrecedence(t *testing.T) {
	cases := []struct {
		name string
		cli  int
		yaml int
		want int
		src  string
	}{
		{"cli wins", 8, 4, 8, SourceCLI},
		{"yaml when cli unset", 0, 4, 4, SourceConfig},
		{"default when both unset", 0, 0, DefaultConcurrency, SourceDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := ResolveInput{
				DetectedLang: builder.Go,
				MatrixFlag:   true,
				CLI:          CLIOptions{GoVersions: []string{"1.26"}, Platforms: []string{"linux/amd64"}, Concurrency: tc.cli},
				YAML:         yamlProfile(builder.Go, nil, nil, tc.yaml),
			}
			prof, _, err := Resolve(in)
			if err != nil {
				t.Fatal(err)
			}
			if prof.Concurrency != tc.want || prof.Source.Concurrency != tc.src {
				t.Fatalf("concurrency = %d (%s), want %d (%s)",
					prof.Concurrency, prof.Source.Concurrency, tc.want, tc.src)
			}
		})
	}
}

// --- Conflict handling ----------------------------------------------------------

func TestResolve_BothEcosystemsRejected(t *testing.T) {
	in := ResolveInput{
		DetectedLang: builder.Go,
		CLI:          CLIOptions{GoVersions: []string{"1.26"}, RustVersions: []string{"1.77"}},
	}
	_, _, err := Resolve(in)
	if err == nil || phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("expected invalid-argument error, got %v", err)
	}
}

func TestResolve_EcosystemsAcrossCLIAndYAMLRejected(t *testing.T) {
	in := ResolveInput{
		DetectedLang: builder.Go,
		CLI:          CLIOptions{GoVersions: []string{"1.26"}},
		YAML:         yamlProfile(builder.Rust, []string{"1.77"}, nil, 0),
		YAMLEnabled:  true,
	}
	_, _, err := Resolve(in)
	if err == nil {
		t.Fatal("go via CLI + rust via YAML must be rejected")
	}
}

func TestResolve_LanguageMismatchRejected(t *testing.T) {
	in := ResolveInput{
		DetectedLang: builder.Go,
		CLI:          CLIOptions{RustVersions: []string{"1.77"}, Platforms: []string{"linux/amd64"}},
	}
	_, _, err := Resolve(in)
	if err == nil {
		t.Fatal("rust versions on a Go project must be rejected, not silently built")
	}
}

func TestResolve_YAMLEcosystemSelectsLanguage(t *testing.T) {
	in := ResolveInput{
		DetectedLang: builder.Rust, // detection agrees with the profile
		YAML:         yamlProfile(builder.Rust, []string{"1.77"}, []string{"linux/amd64"}, 0),
		YAMLEnabled:  true,
	}
	prof, active, err := Resolve(in)
	if err != nil || !active {
		t.Fatalf("err=%v active=%v", err, active)
	}
	if prof.Lang != builder.Rust {
		t.Fatalf("lang = %s, want rust", prof.Lang)
	}
}

// --- Expansion equivalence: YAML and CLI converge to identical plans -------------

func TestResolve_YAMLAndCLIProduceIdenticalPlans(t *testing.T) {
	versions := []string{"1.26", "1.27"}
	platforms := []string{"linux/amd64", "linux/arm64"}

	cliIn := ResolveInput{
		DetectedLang: builder.Go,
		CLI:          CLIOptions{GoVersions: versions, Platforms: platforms},
	}
	yamlIn := ResolveInput{
		DetectedLang: builder.Go,
		YAML:         yamlProfile(builder.Go, versions, platforms, 0),
		YAMLEnabled:  true,
	}

	cliProf, _, err := Resolve(cliIn)
	if err != nil {
		t.Fatal(err)
	}
	yamlProf, _, err := Resolve(yamlIn)
	if err != nil {
		t.Fatal(err)
	}

	cliPlan, err := cliProf.Plan()
	if err != nil {
		t.Fatal(err)
	}
	yamlPlan, err := yamlProf.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(comboIDs(cliPlan), comboIDs(yamlPlan)) {
		t.Fatalf("YAML and CLI plans differ:\nCLI:  %v\nYAML: %v", comboIDs(cliPlan), comboIDs(yamlPlan))
	}
	// And identical to the plain ParsePlan the pure-CLI path always used.
	direct, err := ParsePlan(builder.Go, versions, platforms)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(comboIDs(direct), comboIDs(cliPlan)) {
		t.Fatalf("resolved plan differs from legacy ParsePlan: %v vs %v", comboIDs(cliPlan), comboIDs(direct))
	}
}

// --- Profile.Plan errors ----------------------------------------------------------

func TestProfilePlan_Errors(t *testing.T) {
	noVersions := &Profile{Lang: builder.Go, Platforms: []string{"linux/amd64"}}
	if _, err := noVersions.Plan(); err == nil {
		t.Fatal("expected error for missing versions")
	}
	noPlatforms := &Profile{Lang: builder.Go, Versions: []string{"1.26"}}
	if _, err := noPlatforms.Plan(); err == nil {
		t.Fatal("expected error for missing platforms")
	}
	if _, err := (*Profile)(nil).Plan(); err == nil {
		t.Fatal("expected error for nil profile")
	}
}

// --- IsActive ---------------------------------------------------------------------

func TestIsActive(t *testing.T) {
	cases := []struct {
		name string
		in   ResolveInput
		want bool
	}{
		{"nothing", ResolveInput{DetectedLang: builder.Go}, false},
		{"flag", ResolveInput{DetectedLang: builder.Go, MatrixFlag: true}, true},
		{"go versions", ResolveInput{DetectedLang: builder.Go, CLI: CLIOptions{GoVersions: []string{"1.26"}}}, true},
		{"platforms only", ResolveInput{DetectedLang: builder.Go, CLI: CLIOptions{Platforms: []string{"linux/amd64"}}}, true},
		{"yaml enabled", ResolveInput{DetectedLang: builder.Go, YAMLEnabled: true}, true},
		{"explicit false", ResolveInput{DetectedLang: builder.Go, MatrixFlagSet: true, MatrixFlag: false, YAMLEnabled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsActive(tc.in); got != tc.want {
				t.Fatalf("IsActive = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- RunID ------------------------------------------------------------------------

func TestNewRunID_Format(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		id := NewRunID(now)
		if _, err := ParseRunID(string(id)); err != nil {
			t.Fatalf("minted ID %q rejected by ParseRunID: %v", id, err)
		}
		if want := "mx_20260909_"; len(id) != len(want)+4 || string(id[:len(want)]) != want {
			t.Fatalf("unexpected ID format: %q", id)
		}
	}
}

// TestNewRunID_SequentialEntropy verifies consecutive IDs from a counting
// entropy source are distinct and well-formed (the raw 4-hex space is small
// by design; collision safety is NewUniqueRunID's + SaveRun's contract,
// tested in history_test.go).
func TestNewRunID_SequentialEntropy(t *testing.T) {
	orig := runIDEntropy
	defer func() { runIDEntropy = orig }()
	counter := byte(0)
	runIDEntropy = func(b []byte) (int, error) {
		b[0], b[1] = 0, counter
		counter++
		return len(b), nil
	}
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	seen := make(map[RunID]bool, 16)
	for i := 0; i < 16; i++ {
		id := NewRunID(now)
		if seen[id] {
			t.Fatalf("duplicate ID from sequential entropy: %s", id)
		}
		seen[id] = true
		if _, err := ParseRunID(string(id)); err != nil {
			t.Fatalf("bad ID %q: %v", id, err)
		}
	}
}

func TestParseRunID_RejectsBadInput(t *testing.T) {
	for _, bad := range []string{
		"", "mx_20260909", "mx_20260909_8f3", "mx_20260909_8f311",
		"mx_2026090X_8f31", "MX_20260909_8F31", "mx_20260909_8F31",
		"../../etc/passwd", "mx_20260909_8f3g", "hello",
	} {
		if _, err := ParseRunID(bad); err == nil {
			t.Fatalf("ParseRunID accepted %q", bad)
		}
	}
	if id, err := ParseRunID("mx_20260909_8f31"); err != nil || id != RunID("mx_20260909_8f31") {
		t.Fatalf("ParseRunID rejected valid ID: %v", err)
	}
}
