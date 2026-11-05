package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/env"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/secrets"
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

// doctorJSONView is the machine-contract view of one doctor check. status is
// the external lifecycle vocabulary: pass -> succeeded, warn -> pending
// (usable, attention recommended), fail -> failed.
type doctorJSONView struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type doctorJSONResult struct {
	Checks  []doctorJSONView  `json:"checks"`
	Summary doctorJSONSummary `json:"summary"`
}

type doctorJSONSummary struct {
	Passed   int `json:"passed"`
	Warnings int `json:"warnings"`
	Failed   int `json:"failed"`
}

var DoctorCmd = &cobra.Command{
	Use:           "doctor",
	Short:         "Diagnoses whether the current project is configured to run under Phelix",
	Long:          "Checks project detection, toolchain, phelix.yaml and — most importantly — whether the application listens on the PORT environment variable instead of a hardcoded port. Diagnostic only; never modifies source code.",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {

		if doctorJSON {
			restore := machine.EnterJSON()
			defer restore()
		}

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
		return writeEnvelopeResult(machine.Success("", buildDoctorResult(results, passed, warned, failed)))
	},
}

// buildDoctorResult projects the check results onto the machine contract.
func buildDoctorResult(results []checkResult, passed, warned, failed int) *doctorJSONResult {
	views := make([]doctorJSONView, 0, len(results))
	for _, r := range results {
		status := machine.StatusSucceeded
		switch {
		case r.warn:
			status = machine.StatusPending
		case !r.pass:
			status = machine.StatusFailed
		}
		views = append(views, doctorJSONView{Name: r.name, Status: status, Detail: r.detail})
	}
	return &doctorJSONResult{
		Checks: views,
		Summary: doctorJSONSummary{
			Passed:   passed,
			Warnings: warned,
			Failed:   failed,
		},
	}
}

func init() {
	DoctorCmd.Flags().BoolVar(&doctorJSON, "json", false,
		"Output machine-readable JSON (stdout carries only the result envelope; check output moves to stderr)")
}

var doctorJSON bool

func runDoctorChecks(dir string) []checkResult {
	var results []checkResult

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
		return results
	}

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

	if err := buildMgr.ValidateTools(lang); err == nil {
		results = append(results, checkResult{name: "Build tools", pass: true, detail: "ready"})
	} else {
		results = append(results, checkResult{name: "Build tools", warn: true, detail: strings.TrimSpace(err.Error())})
	}

	cfg, cfgErr := project.Load(dir)
	switch {
	case cfgErr == nil:
		results = append(results, checkResult{name: "phelix.yaml", pass: true, detail: "found"})
		nameDetail := cfg.Name
		if nameDetail == "" {
			nameDetail = "(not set — prompted at build time)"
		}
		results = append(results, checkResult{name: "Application name", pass: true, detail: nameDetail})
		portDetail := fmt.Sprintf("%d", cfg.Port)
		if cfg.Port == 0 {
			portDetail = "(not set — --port or default 8080)"
		}
		results = append(results, checkResult{name: "Configured port", pass: true, detail: portDetail})
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

	results = appendSecretsChecks(results, cfg)

	if cfg != nil && cfg.IsWorker() {
		results = append(results, checkResult{
			name: "PORT configuration", pass: true,
			detail: "not required for type: worker",
		})
		return results
	}

	hits, readsPORT := project.ScanHardcodedPort(dir)
	unsafe := firstHardcodedHit(hits)
	switch {
	case unsafe != nil:
		h := *unsafe
		var b strings.Builder
		b.WriteString(fmt.Sprintf("\nApplication appears to listen on hardcoded port :%d.\n", h.Port))
		b.WriteString(fmt.Sprintf("\nFound in %s:%d:\n    %s\n", h.File, h.Line, h.Snippet))
		b.WriteString("\nPhelix expects the application to read the PORT environment variable.\n")
		b.WriteString("\nExpected behavior:\n\n")
		b.WriteString(portRepairExample(h.File, h.Port))
		b.WriteString(fmt.Sprintf("\nCurrent detected behavior:\n\n    %s\n", h.Snippet))
		b.WriteString("\nHint:\n  Make the application respect the PORT environment variable.\n")
		results = append(results, checkResult{
			name: "PORT configuration", detail: fmt.Sprintf("hardcoded :%d", h.Port),
			message: b.String(),
		})
	case len(hits) > 0:
		h := hits[0]
		results = append(results, checkResult{
			name: "PORT configuration", warn: true,
			detail: fmt.Sprintf("possible hardcoded :%d in %s:%d (same file reads PORT)", h.Port, h.File, h.Line),
		})
	case readsPORT:
		results = append(results, checkResult{
			name: "PORT configuration", pass: true, detail: "reads PORT environment variable",
		})
	default:
		results = append(results, checkResult{
			name: "PORT configuration", warn: true,
			detail: "could not be verified automatically",
			message: "\nNo hardcoded listener and no PORT environment variable usage were found.\n" +
				"If the application is a server, make sure it listens on $PORT.\n",
		})
	}

	return results
}

// firstHardcodedHit returns the first listener whose own file does not read
// PORT. A PORT read somewhere else in the project must not excuse it.
func firstHardcodedHit(hits []project.HardcodedPort) *project.HardcodedPort {
	for i := range hits {
		if !hits[i].ReadsPORT {
			return &hits[i]
		}
	}
	return nil
}

// portRepairExample renders the minimal PORT idiom for the listener's
// language, so Rust projects do not receive Go-only advice.
func portRepairExample(file string, port int) string {
	if strings.HasSuffix(file, ".rs") {
		return fmt.Sprintf("    let port = std::env::var(\"PORT\").unwrap_or_else(|_| \"%d\".to_string());\n\n"+
			"    let listener = TcpListener::bind(format!(\"127.0.0.1:{}\", port))?;\n", port)
	}
	return fmt.Sprintf("    port := os.Getenv(\"PORT\")\n    if port == \"\" {\n        port = \"%d\"\n    }\n\n    http.ListenAndServe(\":\"+port, ...)\n", port)
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

// appendSecretsChecks reports on the local secret management layer when the
// project declares imports: the master key, whether the application's store
// can satisfy every import, and whether any import collides with a plain
// environment variable. Never prints a value; only names, fingerprints and
// presence.
func appendSecretsChecks(results []checkResult, cfg *project.Config) []checkResult {
	if cfg == nil {
		return results
	}
	var info *app.AppInfo
	if manager, ok := app.Manager.(*app.AppManager); ok {
		if err := manager.LoadState(); err != nil {
			return append(results, checkResult{name: "Secrets", detail: "app state unreadable"})
		}
		for _, candidate := range manager.Apps {
			if candidate.Name == cfg.Name {
				info = candidate
				break
			}
		}
	}
	var imports []secrets.Binding
	if cfg.Secrets != nil {
		imports = cfg.Secrets.Imports
	}
	if info == nil {
		if len(imports) > 0 {
			results = append(results, checkResult{name: "Secrets imports", warn: true, detail: "application is not registered yet"})
		}
		return results
	}
	st, err := secrets.Open(info.ID)
	if err != nil {
		return append(results, checkResult{name: "Secrets store", detail: "unreadable or unsafe permissions", message: err.Error()})
	}
	if len(imports) == 0 && len(st.Bindings) == 0 && st.Pending == nil && len(st.Secrets) == 0 {
		return results
	}
	results = append(results, checkResult{name: "Secrets permissions", pass: true, detail: "directory 0700, store and lock 0600"})
	if _, err := env.GetMasterKey(); err != nil {
		results = append(results, checkResult{name: "Secrets master key", detail: "unreadable", message: err.Error()})
	} else {
		results = append(results, checkResult{name: "Secrets master key", pass: true, detail: "readable"})
	}
	for _, binding := range append(append([]secrets.Binding{}, imports...), st.Bindings...) {
		_, err := secrets.Resolve(context.Background(), info.ID, binding.Secret)
		check := checkResult{name: "Secret " + binding.Secret, pass: err == nil, detail: "resolvable as " + binding.Env}
		if err != nil {
			check.detail = "cannot resolve as " + binding.Env
			check.message = err.Error()
		}
		results = append(results, check)
	}
	if err := secrets.CheckCollisions(info.ID, imports); err != nil {
		results = append(results, checkResult{name: "Secrets imports", warn: true, detail: "binding conflict", message: err.Error()})
	}
	if st.Pending != nil {
		check := checkResult{name: "Secrets pending transaction", warn: true, detail: "awaiting reconciliation; run phelix secrets apply"}
		if _, err := deploy.Load(info.Name); err != nil {
			check.warn = false
			check.detail = "journal unreadable; pending retained"
			check.message = err.Error()
		}
		results = append(results, check)
	}
	err = deploy.RefreshSecretCoverage(app.AppListItem{ID: info.ID, Name: info.Name, PID: info.PID, Status: info.Status, SecretEnvs: info.SecretEnvs})
	state, reason := secrets.SecurityOf(info.ID)
	results = append(results, checkResult{name: "Secrets security", pass: err == nil, detail: string(state), message: reason})
	return results
}
