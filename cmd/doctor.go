package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/toolchain"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// checkResult is one doctor diagnostic line.
type checkResult struct {
	name    string
	pass    bool
	warn    bool
	detail  string
	message string // extra explanation block for failures
}

var DoctorCmd = &cobra.Command{
	Use:           "doctor",
	Short:         "Diagnoses whether the current project is configured to run under Phelix",
	Long:          "Checks project detection, toolchain, phelix.yaml and — most importantly — whether the application listens on the PORT environment variable instead of a hardcoded port. Diagnostic only; never modifies source code.",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := os.Getwd()
		if err != nil {
			return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to get current directory", err)
		}

		fmt.Println("Phelix Doctor")
		fmt.Println()

		results := runDoctorChecks(dir)

		passed, failed, warned := 0, 0, 0
		for _, r := range results {
			switch {
			case r.pass:
				passed++
				fmt.Printf("%s %-24s %s\n", color.GreenString("✓"), r.name, r.detail)
			case r.warn:
				warned++
				fmt.Printf("%s %-24s %s\n", color.YellowString("⚠"), r.name, r.detail)
			default:
				failed++
				fmt.Printf("%s %-24s %s\n", color.RedString("✗"), r.name, r.detail)
			}
			if r.message != "" {
				fmt.Println(r.message)
			}
		}

		fmt.Println()
		fmt.Println("Summary:")
		fmt.Printf("  %d passed\n", passed)
		if warned > 0 {
			fmt.Printf("  %d warning(s)\n", warned)
		}
		fmt.Printf("  %d failed\n", failed)

		if failed > 0 {
			fmt.Println()
			fmt.Println("Your application is not currently Phelix-compatible.")
			return phelixerr.New(phelixerr.CodeValidation, "doctor found failing checks")
		}
		return nil
	},
}

func init() {
	DoctorCmd.Flags().BoolVar(&buildDebug, "debug", false, "Show verbose diagnostics")
	_ = buildDebug // shared flag var; no extra behavior yet
}

func runDoctorChecks(dir string) []checkResult {
	var results []checkResult

	// --- Project detection ---
	buildMgr := builder.NewBuildManager()
	lang := buildMgr.DetectLanguage(dir)
	if lang.IsSupported() {
		results = append(results, checkResult{
			name: "Project detected", pass: true,
			detail: fmt.Sprintf("%s (%s)", lang, dir),
		})
	} else {
		results = append(results, checkResult{
			name:    "Project detected",
			detail:  "not found",
			message: "\nNo Go or Rust project detected in this directory.\nA go.mod (Go) or Cargo.toml (Rust) is required.\n",
		})
		return results // nothing else can run without a project
	}

	// --- Toolchain ---
	if toolchain.IsInstalled(lang) {
		results = append(results, checkResult{
			name:   langToolchainName(lang),
			pass:   true,
			detail: toolchainVersion(lang),
		})
	} else {
		results = append(results, checkResult{
			name:    langToolchainName(lang),
			detail:  "not available",
			message: "\nInstall the toolchain, or run any phelix build command and accept the install prompt.\n",
		})
	}

	// --- Buildability ---
	if err := buildMgr.ValidateTools(lang); err == nil {
		results = append(results, checkResult{name: "Build tools", pass: true, detail: "ready"})
	} else {
		results = append(results, checkResult{name: "Build tools", warn: true, detail: strings.TrimSpace(err.Error())})
	}

	// --- phelix.yaml ---
	cfg, cfgErr := project.Load(dir)
	switch {
	case cfgErr == nil:
		results = append(results, checkResult{name: "phelix.yaml", pass: true, detail: "found"})
		results = append(results, checkResult{name: "Application name", pass: true, detail: cfg.Name})
		results = append(results, checkResult{name: "Configured port", pass: true, detail: fmt.Sprintf("%d", cfg.Port)})
	case phelixerr.AsError(cfgErr) != nil && phelixerr.AsError(cfgErr).Code == phelixerr.CodeNotFound:
		results = append(results, checkResult{
			name:   "phelix.yaml",
			warn:   true,
			detail: "not found (optional)",
			message: "\nRun 'phelix init' to create it. Projects without phelix.yaml still work;\n" +
				"the port then comes from --port or the default (8080).\n",
		})
	default:
		results = append(results, checkResult{
			name: "phelix.yaml", detail: "invalid",
			message: "\n" + cfgErr.Error() + "\n",
		})
	}

	// --- PORT configuration (the key diagnostic) ---
	hits, readsPORT := project.ScanHardcodedPort(dir)
	switch {
	case len(hits) > 0 && !readsPORT:
		var b strings.Builder
		h := hits[0]
		b.WriteString(fmt.Sprintf("\nApplication appears to listen on hardcoded port :%d.\n", h.Port))
		b.WriteString(fmt.Sprintf("\nFound in %s:%d:\n    %s\n", h.File, h.Line, h.Snippet))
		b.WriteString("\nPhelix expects the application to read the PORT environment variable.\n")
		b.WriteString("\nExpected behavior:\n\n")
		b.WriteString(fmt.Sprintf("    port := os.Getenv(\"PORT\")\n    if port == \"\" {\n        port = \"%d\"\n    }\n\n    http.ListenAndServe(\":\"+port, ...)\n", h.Port))
		b.WriteString(fmt.Sprintf("\nCurrent detected behavior:\n\n    %s\n", h.Snippet))
		b.WriteString("\nHint:\n  Make the application respect the PORT environment variable.\n")
		results = append(results, checkResult{
			name: "PORT configuration", detail: fmt.Sprintf("hardcoded :%d", h.Port),
			message: b.String(),
		})
	case len(hits) > 0 && readsPORT:
		// Reads PORT somewhere but also has literal listeners; may be a default
		// fallback like ListenAndServe(":"+port). Not confident enough to fail.
		h := hits[0]
		results = append(results, checkResult{
			name: "PORT configuration", warn: true,
			detail: fmt.Sprintf("possible hardcoded :%d in %s:%d (reads PORT elsewhere)", h.Port, h.File, h.Line),
		})
	case readsPORT:
		results = append(results, checkResult{
			name: "PORT configuration", pass: true, detail: "reads PORT environment variable",
		})
	default:
		results = append(results, checkResult{
			name: "PORT configuration", warn: true,
			detail: "could not be verified automatically",
			message: "\nNo hardcoded listener and no os.Getenv(\"PORT\") usage were found.\n" +
				"If the application is a server, make sure it listens on $PORT.\n",
		})
	}

	return results
}

// langToolchainName renders the toolchain check label.
func langToolchainName(lang builder.Language) string {
	return fmt.Sprintf("%s toolchain", lang)
}

// toolchainVersion returns the primary tool binary's version string, or
// "installed" when the version command fails. Binaries are fixed per language;
// no user input reaches exec.
func toolchainVersion(lang builder.Language) string {
	var out []byte
	var err error
	switch lang {
	case builder.Rust:
		out, err = exec.Command("cargo", "--version").Output()
	default:
		out, err = exec.Command("go", "version").Output()
	}
	if err != nil {
		return "installed"
	}
	return strings.TrimSpace(string(out))
}
