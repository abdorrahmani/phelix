package cmd

import (
	"fmt"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/matrix"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// wizardVersionSuggestions offers recent toolchain versions as checkboxes.
// Versions are open-ended by design (major.minor[.patch]); these are only
// suggestions — "Other (enter manually)" always allows anything else.
var wizardVersionSuggestions = map[builder.Language][]string{
	builder.Go:   {"1.25", "1.26", "1.27"},
	builder.Rust: {"1.78", "1.79", "1.80"},
}

const wizardCustomVersions = "Other (enter manually)"

var matrixInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Interactive wizard: configure the Build Matrix in phelix.yaml",
	Long: `Guides you through creating a matrix profile (ecosystem, versions,
platforms, concurrency) and writes it into phelix.yaml. Existing
configuration is preserved; an existing matrix profile can be edited,
kept, or disabled. The preview uses the real matrix expansion, so it shows
exactly what 'phelix build' would execute.

This command is fully interactive — it does nothing when stdin is not a TTY.`,
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !IsInteractive() {
			return phelixerr.Newf(phelixerr.CodeInvalidArgument,
				"matrix wizard requires an interactive terminal — edit phelix.yaml directly or pass CLI flags (e.g. 'phelix build --matrix --go-versions 1.26 --platforms linux/amd64')")
		}
		return runMatrixWizard()
	},
}

// runMatrixWizard drives the interactive flow. The prompt I/O lives here;
// everything that decides or writes is in the small helpers below so the
// model logic stays unit-testable without a terminal.
func runMatrixWizard() error {
	dir := currentDirOrError()
	if dir == "" {
		return phelixerr.New(phelixerr.CodeFilesystem, "failed to get current directory")
	}

	// A present-but-invalid phelix.yaml is an error, exactly like build: the
	// wizard must never silently build on top of a broken file.
	projCfg, err := loadProjectConfig()
	if err != nil {
		return err
	}

	fmt.Printf("Phelix Matrix Configuration\n\n")

	if projCfg != nil && projCfg.Matrix != nil && matrixProfileHasContent(projCfg.Matrix) {
		action, err := promptExistingMatrixAction(projCfg.Matrix)
		if err != nil {
			return err
		}
		switch action {
		case "Keep existing":
			fmt.Printf("%s Matrix profile unchanged.\n", color.GreenString("✓"))
			return nil
		case "Disable matrix":
			disabled := *projCfg.Matrix
			disabled.Enabled = false
			if err := project.SaveMatrixConfig(dir, &disabled); err != nil {
				return err
			}
			fmt.Printf("%s Matrix disabled in %s (dimensions kept for later re-enable).\n",
				color.GreenString("✓"), color.CyanString(project.FileName))
			return nil
		case "Cancel":
			fmt.Println("Cancelled.")
			return nil
		}
		// "Edit" falls through to the configuration flow below.
	}

	// Detect the ecosystem so the wizard can default to it.
	buildMgr := builder.NewBuildManager()
	lang := buildMgr.DetectLanguage(dir)
	if lang != builder.Rust {
		lang = builder.Go
	}

	lang, err = promptMatrixEcosystem(lang)
	if err != nil {
		return err
	}
	versions, err := promptMatrixVersions(lang)
	if err != nil {
		return err
	}
	platforms, err := promptMatrixPlatforms()
	if err != nil {
		return err
	}
	concurrency, err := PromptInt("Concurrency (parallel builds)", matrix.DefaultConcurrency)
	if err != nil {
		return err
	}

	cfg := wizardMatrixConfig(lang, versions, platforms, concurrency)

	// Preview through the real expansion: the profile is converted exactly
	// like phelix.yaml is at build time, and the plan comes from the same
	// ParsePlan the engine uses. No second combination calculation.
	plan, err := cfg.MatrixProfile().Plan()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeInvalidArgument, "invalid matrix configuration", err)
	}

	fmt.Print(matrixWizardPreview(plan, concurrency))

	save, err := PromptConfirm("Save this configuration to "+project.FileName+"?", true)
	if err != nil {
		return err
	}
	if !save {
		fmt.Println("Cancelled — nothing written.")
		return nil
	}

	if err := project.SaveMatrixConfig(dir, cfg); err != nil {
		return err
	}
	fmt.Printf("%s Matrix profile saved to %s — 'phelix build' will now use it.\n",
		color.GreenString("✓"), color.CyanString(project.FileName))
	return nil
}

// matrixProfileHasContent reports whether the matrix section configures
// anything at all (an empty `matrix: {}` is treated as "no profile yet").
func matrixProfileHasContent(m *project.MatrixConfig) bool {
	if m == nil {
		return false
	}
	return m.Enabled ||
		(m.Go != nil && len(m.Go.Versions) > 0) ||
		(m.Rust != nil && len(m.Rust.Versions) > 0) ||
		len(m.Platforms) > 0
}

// promptExistingMatrixAction shows the current profile and asks what to do.
func promptExistingMatrixAction(m *project.MatrixConfig) (string, error) {
	fmt.Printf("Existing Matrix Profile found in %s:\n\n", project.FileName)
	fmt.Print(describeMatrixConfig(m))
	fmt.Println()
	return PromptSelect("What would you like to do?", []string{
		"Edit",
		"Keep existing",
		"Disable matrix",
		"Cancel",
	})
}

// promptMatrixEcosystem asks which ecosystem the matrix builds; the detected
// one is the preselected first option.
func promptMatrixEcosystem(detected builder.Language) (builder.Language, error) {
	options := []string{"Go", "Rust"}
	if detected == builder.Rust {
		options = []string{"Rust", "Go"}
	}
	chosen, err := PromptSelect("Build ecosystem:", options)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(chosen, "rust") {
		return builder.Rust, nil
	}
	return builder.Go, nil
}

// promptMatrixVersions asks for the toolchain versions, validating each
// answer with the engine's own ValidateVersions before accepting it.
func promptMatrixVersions(lang builder.Language) ([]string, error) {
	suggestions := wizardVersionSuggestions[lang]
	if suggestions == nil {
		suggestions = wizardVersionSuggestions[builder.Go]
	}
	opts := append(append([]string{}, suggestions...), wizardCustomVersions)

	for {
		var picked []string
		prompt := &survey.MultiSelect{
			Message: fmt.Sprintf("%s versions:", lang),
			Options: opts,
		}
		if err := survey.AskOne(prompt, &picked); err != nil {
			return nil, phelixerr.Wrap(phelixerr.CodeInvalidArgument, "selection cancelled", err)
		}

		if len(picked) == 1 && picked[0] == wizardCustomVersions {
			raw, err := PromptString(fmt.Sprintf("%s versions (comma-separated, e.g. %s)",
				lang, strings.Join(suggestions, ",")), "")
			if err != nil {
				return nil, err
			}
			if raw != "" {
				picked = strings.Split(raw, ",")
			}
		}

		versions := make([]string, 0, len(picked))
		for _, v := range picked {
			if v != wizardCustomVersions {
				versions = append(versions, v)
			}
		}
		if len(versions) == 0 {
			fmt.Printf("  %s Select at least one version.\n", color.YellowString("⚠"))
			continue
		}
		if _, err := matrix.ValidateVersions(lang, versions); err != nil {
			fmt.Printf("  %s %v\n", color.YellowString("⚠"), err)
			continue
		}
		return versions, nil
	}
}

// promptMatrixPlatforms asks for the target platforms from the engine's
// supported list, validating with ValidatePlatforms before accepting.
func promptMatrixPlatforms() ([]string, error) {
	known := matrix.KnownPlatforms()
	for {
		var picked []string
		prompt := &survey.MultiSelect{
			Message: "Platforms:",
			Options: known,
		}
		if err := survey.AskOne(prompt, &picked); err != nil {
			return nil, phelixerr.Wrap(phelixerr.CodeInvalidArgument, "selection cancelled", err)
		}
		if len(picked) == 0 {
			fmt.Printf("  %s Select at least one platform.\n", color.YellowString("⚠"))
			continue
		}
		if _, err := matrix.ValidatePlatforms(picked); err != nil {
			fmt.Printf("  %s %v\n", color.YellowString("⚠"), err)
			continue
		}
		return picked, nil
	}
}

// wizardMatrixConfig builds the phelix.yaml matrix section from the wizard's
// answers — the same project.MatrixConfig type the YAML parser produces, so
// the wizard output is indistinguishable from a hand-written profile.
func wizardMatrixConfig(lang builder.Language, versions, platforms []string, concurrency int) *project.MatrixConfig {
	cfg := &project.MatrixConfig{
		Enabled:     true,
		Platforms:   platforms,
		Concurrency: concurrency,
	}
	if concurrency <= 0 {
		cfg.Concurrency = matrix.DefaultConcurrency
	}
	wrapped := make([]project.MatrixVersion, 0, len(versions))
	for _, v := range versions {
		wrapped = append(wrapped, project.MatrixVersion(v))
	}
	tc := &project.MatrixToolchain{Versions: wrapped}
	if lang == builder.Rust {
		cfg.Rust = tc
	} else {
		cfg.Go = tc
	}
	return cfg
}

// matrixWizardPreview renders the preview block. The combination list comes
// from the actual expanded plan — never from a versions×platforms
// multiplication.
func matrixWizardPreview(plan *matrix.MatrixPlan, concurrency int) string {
	var b strings.Builder
	b.WriteString("\nMatrix Preview\n\n")
	fmt.Fprintf(&b, "  %s versions\n", strings.Join(planVersions(plan), ", "))
	fmt.Fprintf(&b, "  Platforms:  %s\n", strings.Join(planPlatforms(plan), ", "))
	fmt.Fprintf(&b, "  Concurrency: %d\n", concurrency)
	fmt.Fprintf(&b, "\n  Total combinations: %d\n\n", len(plan.Combinations))
	dim := color.New(color.Faint)
	for _, c := range plan.Combinations {
		fmt.Fprintf(&b, "    %s %s\n", dim.Sprint("•"), c.ID())
	}
	b.WriteString("\n")
	return b.String()
}

// planVersions/planPlatforms recover the (deduplicated, ordered) dimension
// lists from an expanded plan so previews never recompute them from raw
// input.
func planVersions(plan *matrix.MatrixPlan) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range plan.Combinations {
		if !seen[c.Version] {
			seen[c.Version] = true
			out = append(out, c.Version)
		}
	}
	return out
}

func planPlatforms(plan *matrix.MatrixPlan) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range plan.Combinations {
		if !seen[c.Platform] {
			seen[c.Platform] = true
			out = append(out, c.Platform)
		}
	}
	return out
}

// describeMatrixConfig renders a matrix profile for display (wizard and
// future reuse).
func describeMatrixConfig(m *project.MatrixConfig) string {
	var b strings.Builder
	if m.Go != nil && len(m.Go.Versions) > 0 {
		versions := make([]string, 0, len(m.Go.Versions))
		for _, v := range m.Go.Versions {
			versions = append(versions, string(v))
		}
		fmt.Fprintf(&b, "  Go versions:  %s\n", strings.Join(versions, ", "))
	}
	if m.Rust != nil && len(m.Rust.Versions) > 0 {
		versions := make([]string, 0, len(m.Rust.Versions))
		for _, v := range m.Rust.Versions {
			versions = append(versions, string(v))
		}
		fmt.Fprintf(&b, "  Rust versions: %s\n", strings.Join(versions, ", "))
	}
	fmt.Fprintf(&b, "  Platforms:    %s\n", strings.Join(m.Platforms, ", "))
	if m.Concurrency > 0 {
		fmt.Fprintf(&b, "  Concurrency:  %d\n", m.Concurrency)
	}
	fmt.Fprintf(&b, "  Enabled:      %v\n", m.Enabled)
	return b.String()
}
