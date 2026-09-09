package matrix

import (
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// Dimension origins, recorded on a Profile so a Matrix Run snapshot can say
// where each effective value came from.
const (
	SourceCLI      = "cli"
	SourceConfig   = "phelix.yaml"
	SourceDefault  = "default"
	SourceDetected = "detected"
)

// Profile is the normalized matrix configuration: the single internal
// representation that CLI flags, the phelix.yaml matrix profile, and the
// interactive wizard all converge into before validation and expansion. There
// is no separate "YAML matrix" or "CLI matrix" path downstream of this type.
type Profile struct {
	Lang        builder.Language
	Versions    []string
	Platforms   []string
	Concurrency int
	// Include/Exclude are the effective matrix rules. They have no CLI flags
	// (rules with partial matching are not flag-friendly); they come from the
	// phelix.yaml profile or the wizard and are part of the run snapshot.
	Include []Rule
	Exclude []Rule
	// Retries is the automatic-retry budget: how many additional times a
	// failed combination is retried inside the same run (0 = no retry).
	Retries int
	// Source records the origin of each dimension ("cli", "phelix.yaml",
	// "default", "detected") for run snapshots and `matrix show`.
	Source ProfileSource
}

// ProfileSource documents where each Profile dimension came from.
type ProfileSource struct {
	Lang        string
	Versions    string
	Platforms   string
	Concurrency string
	Retries     string
	Include     string
	Exclude     string
}

// CLIOptions carries the matrix-related CLI flag values into Resolve. Nil/zero
// means "not specified on the command line" — the cmd layer only fills
// Concurrency when the --matrix-concurrency flag was explicitly set, since its
// default value equals the engine default and is indistinguishable otherwise.
type CLIOptions struct {
	GoVersions   []string
	RustVersions []string
	Platforms    []string
	Concurrency  int
	Retries      int
}

// ResolveInput bundles everything the convergence needs. MatrixFlagSet tells
// Resolve whether --matrix was passed at all: an explicit --matrix=false
// disables the matrix even when the phelix.yaml profile is enabled (CLI
// explicit > YAML > default, with no ambiguous cases).
type ResolveInput struct {
	DetectedLang  builder.Language
	MatrixFlag    bool
	MatrixFlagSet bool
	CLI           CLIOptions
	// YAML is the normalized profile from phelix.yaml (nil when the file has
	// no matrix section). Its Enabled flag is passed separately so the
	// normalized Profile stays free of YAML-only concerns.
	YAML        *Profile
	YAMLEnabled bool
}

// IsActive reports whether matrix mode is active under the exact rules
// Resolve applies: an explicit --matrix=false disables it; otherwise the
// flag, any dimension flag, or an enabled phelix.yaml profile activates it.
func IsActive(in ResolveInput) bool {
	if in.MatrixFlagSet && !in.MatrixFlag {
		return false
	}
	return in.MatrixFlag ||
		len(in.CLI.GoVersions) > 0 || len(in.CLI.RustVersions) > 0 || len(in.CLI.Platforms) > 0 ||
		in.YAMLEnabled
}

// Resolve converges CLI flags, the phelix.yaml profile, and command defaults
// into one normalized Profile. Precedence per dimension is strict:
//
//	CLI explicit value > phelix.yaml matrix profile > command default
//
// List dimensions (versions, platforms) are replaced, never merged: passing
// --go-versions next to a configured profile overrides the whole version list,
// and unmentioned dimensions keep their configured values.
//
// It returns (nil, false, nil) when the matrix is not active at all.
func Resolve(in ResolveInput) (*Profile, bool, error) {
	// Is matrix mode active? An explicit --matrix=false wins over everything;
	// otherwise the flag, any dimension flag, or an enabled YAML profile
	// activates it.
	if !IsActive(in) {
		return nil, false, nil
	}

	prof := &Profile{Concurrency: DefaultConcurrency, Source: ProfileSource{
		Lang: SourceDetected, Versions: SourceDefault, Platforms: SourceDefault, Concurrency: SourceDefault,
	}}
	// --- Language: explicit version lists (CLI first, then YAML) override
	// detection. Specifying both ecosystems is ambiguous and rejected.
	goSrc, rustSrc := "", ""
	if len(in.CLI.GoVersions) > 0 {
		goSrc = SourceCLI
	}
	if len(in.CLI.RustVersions) > 0 {
		rustSrc = SourceCLI
	}
	if goSrc == "" && in.YAML != nil && len(in.YAML.Versions) > 0 && in.YAML.Lang == builder.Go {
		goSrc = SourceConfig
	}
	if rustSrc == "" && in.YAML != nil && len(in.YAML.Versions) > 0 && in.YAML.Lang == builder.Rust {
		rustSrc = SourceConfig
	}
	switch {
	case goSrc != "" && rustSrc != "":
		return nil, false, phelixerr.New(phelixerr.CodeInvalidArgument,
			"matrix: go and rust versions are mutually exclusive — specify only one ecosystem")
	case goSrc != "":
		prof.Lang = builder.Go
		prof.Source.Lang = goSrc
	case rustSrc != "":
		prof.Lang = builder.Rust
		prof.Source.Lang = rustSrc
	default:
		prof.Lang = in.DetectedLang
	}

	// An explicitly configured ecosystem that disagrees with the detected
	// project language is a misconfiguration: silently building the wrong
	// toolchain (the old behavior silently ignored --rust-versions on Go
	// projects) hides the mistake behind confusing per-combination failures.
	if (goSrc != "" || rustSrc != "") && prof.Lang != in.DetectedLang {
		return nil, false, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: %s versions configured but the detected project language is %s",
			prof.Lang, in.DetectedLang)
	}

	// --- Versions: CLI replaces YAML (never merged).
	if prof.Lang == builder.Rust {
		if len(in.CLI.RustVersions) > 0 {
			prof.Versions = in.CLI.RustVersions
			prof.Source.Versions = SourceCLI
		} else if in.YAML != nil && in.YAML.Lang == builder.Rust && len(in.YAML.Versions) > 0 {
			prof.Versions = in.YAML.Versions
			prof.Source.Versions = SourceConfig
		}
	} else {
		if len(in.CLI.GoVersions) > 0 {
			prof.Versions = in.CLI.GoVersions
			prof.Source.Versions = SourceCLI
		} else if in.YAML != nil && in.YAML.Lang == builder.Go && len(in.YAML.Versions) > 0 {
			prof.Versions = in.YAML.Versions
			prof.Source.Versions = SourceConfig
		}
	}

	// --- Platforms: CLI replaces YAML (never merged).
	if len(in.CLI.Platforms) > 0 {
		prof.Platforms = in.CLI.Platforms
		prof.Source.Platforms = SourceCLI
	} else if in.YAML != nil && len(in.YAML.Platforms) > 0 {
		prof.Platforms = in.YAML.Platforms
		prof.Source.Platforms = SourceConfig
	}

	// --- Concurrency: explicit CLI flag > YAML > engine default.
	if in.CLI.Concurrency > 0 {
		prof.Concurrency = in.CLI.Concurrency
		prof.Source.Concurrency = SourceCLI
	} else if in.YAML != nil && in.YAML.Concurrency > 0 {
		prof.Concurrency = in.YAML.Concurrency
		prof.Source.Concurrency = SourceConfig
	}
	if prof.Concurrency <= 0 {
		prof.Concurrency = DefaultConcurrency
		prof.Source.Concurrency = SourceDefault
	}

	// --- Retries: explicit CLI flag > YAML > no retry. Retries is execution
	// policy (per-run automatic retry budget), not a build dimension, but it
	// follows the same precedence so the run snapshot can record its origin.
	if in.CLI.Retries > 0 {
		prof.Retries = in.CLI.Retries
		prof.Source.Retries = SourceCLI
	} else if in.YAML != nil && in.YAML.Retries > 0 {
		prof.Retries = in.YAML.Retries
		prof.Source.Retries = SourceConfig
	}
	if prof.Retries < 0 {
		prof.Retries = 0
	}

	// --- Include/Exclude: YAML (or wizard) only — there are no CLI flags for
	// partial-matching rules. CLI versions/platforms still replace the YAML
	// dimensions, but the rules always apply to whatever dimensions are
	// effective, exactly like the documented pipeline.
	if in.YAML != nil {
		prof.Include = in.YAML.Include
		prof.Exclude = in.YAML.Exclude
		if len(prof.Include) > 0 {
			prof.Source.Include = SourceConfig
		}
		if len(prof.Exclude) > 0 {
			prof.Source.Exclude = SourceConfig
		}
	}

	return prof, true, nil
}

// Plan validates the profile through the same Expand engine used by pure-CLI
// matrix builds and expands it into the executable combination list (base
// Cartesian product → include → exclude). This is the single expansion path —
// YAML, CLI, and wizard profiles all produce identical MatrixPlans for
// identical dimensions and rules.
func (p *Profile) Plan() (*MatrixPlan, error) {
	if p == nil {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "matrix: no profile")
	}
	if len(p.Versions) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: no versions configured — pass --go-versions/--rust-versions or set matrix.go.versions/matrix.rust.versions in phelix.yaml")
	}
	if len(p.Platforms) == 0 {
		return nil, phelixerr.Newf(phelixerr.CodeInvalidArgument,
			"matrix: no platforms configured — pass --platforms or set matrix.platforms in phelix.yaml")
	}
	return Expand(p.Lang, p.Versions, p.Platforms, RuleSet{Include: p.Include, Exclude: p.Exclude})
}
