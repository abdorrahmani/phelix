package errreport

import (
	"errors"
	"strings"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func stubInstallCommand(t *testing.T, goCmd, rustCmd string) {
	t.Helper()
	old := InstallCommand
	InstallCommand = func(lang builder.Language) string {
		switch lang {
		case builder.Go:
			return goCmd
		case builder.Rust:
			return rustCmd
		}
		return ""
	}
	t.Cleanup(func() { InstallCommand = old })
}

// TestToolchainResolverGo covers the chains real call sites build:
// toolchain.EnsureTool / BuildManager.ValidateTools produce a structured
// TOOLCHAIN_NOT_FOUND naming the toolchain, which cmd wraps again.
func TestToolchainResolverGo(t *testing.T) {
	stubInstallCommand(t, "sudo apt -y install golang", "")

	err := phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain check failed",
		phelixerr.Newf(phelixerr.CodeToolchainNotFound,
			"%s not installed and installation was declined.\n\n%s",
			"Go toolchain (go)", "Please install Go manually:\n  ..."))

	rep, ok := For(err)
	if !ok {
		t.Fatalf("Go toolchain error not recognized")
	}
	if rep.Title != "Go toolchain not found" {
		t.Fatalf("Title = %q", rep.Title)
	}
	if rep.Command != "sudo apt -y install golang" {
		t.Fatalf("Command = %q", rep.Command)
	}
	if rep.DocsURL != "https://go.dev/doc/install" {
		t.Fatalf("DocsURL = %q", rep.DocsURL)
	}
	if rep.Code != phelixerr.CodeToolchainNotFound {
		t.Fatalf("Code = %s", rep.Code)
	}
	if strings.Contains(rep.Explanation+rep.Suggestion, "works on every") {
		t.Fatalf("suggestion must not claim distro-independence")
	}
}

func TestToolchainResolverGoWithoutPackageManager(t *testing.T) {
	stubInstallCommand(t, "", "")

	err := phelixerr.New(phelixerr.CodeToolchainNotFound,
		"Go toolchain not found. Please install Go from https://golang.org/dl")
	rep, ok := For(err)
	if !ok {
		t.Fatalf("Go toolchain error not recognized")
	}
	if rep.Command != "" {
		t.Fatalf("no deterministic command should be fabricated, got %q", rep.Command)
	}
	if !strings.Contains(rep.Suggestion, "https://go.dev/dl/") {
		t.Fatalf("fallback should point at the official download: %q", rep.Suggestion)
	}
}

func TestToolchainResolverRust(t *testing.T) {
	stubInstallCommand(t, "", "curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh")

	cases := []struct {
		name string
		err  error
	}{
		{"validate tools message", phelixerr.New(phelixerr.CodeToolchainNotFound,
			"Rust toolchain not found. Please install Rust from https://rustup.rs")},
		{"declined install prompt", phelixerr.Wrap(phelixerr.CodeToolchainNotFound, "toolchain check failed",
			phelixerr.Newf(phelixerr.CodeToolchainNotFound,
				"%s not installed and installation was declined.",
				"Rust toolchain (cargo/rustup)"))},
		{"rustup prerequisite missing", phelixerr.New(phelixerr.CodeToolchainNotFound,
			"curl is required to install rustup but was not found")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, ok := For(tc.err)
			if !ok {
				t.Fatalf("Rust toolchain error not recognized")
			}
			if rep.Title != "Rust toolchain not found" {
				t.Fatalf("Title = %q", rep.Title)
			}
			if !strings.Contains(rep.Command, "sh.rustup.rs") {
				t.Fatalf("Command = %q", rep.Command)
			}
			if rep.DocsURL != "https://www.rust-lang.org/tools/install" {
				t.Fatalf("DocsURL = %q", rep.DocsURL)
			}
		})
	}
}

func TestToolchainResolverRustWithoutCommand(t *testing.T) {
	stubInstallCommand(t, "", "")
	err := phelixerr.New(phelixerr.CodeToolchainNotFound, "Rust toolchain not found.")
	rep, ok := For(err)
	if !ok {
		t.Fatalf("Rust toolchain error not recognized")
	}
	if rep.Command != "" {
		t.Fatalf("no deterministic command should be fabricated, got %q", rep.Command)
	}
	if !strings.Contains(rep.Suggestion, "https://rustup.rs/") {
		t.Fatalf("fallback should point at rustup.rs: %q", rep.Suggestion)
	}
}

func TestToolchainResolverIgnoresOtherCodes(t *testing.T) {
	// Wrong code with matching text must not classify (code is the primary
	// signal; text only disambiguates within the code).
	err := phelixerr.New(phelixerr.CodeBuildFailed, "the Go toolchain is too old")
	if _, ok := For(err); ok {
		t.Fatalf("BUILD_FAILED with toolchain-ish text must not produce an install report")
	}
	// Also: a chain that merely mentions "golang" in a URL.
	err = phelixerr.New(phelixerr.CodeConfiguration, "invalid golang setting")
	if _, ok := For(err); ok {
		t.Fatalf("configuration error must not produce a toolchain report")
	}
}

func TestToolchainResolverUnidentifiedToolchainStaysUnknown(t *testing.T) {
	err := errors.New("the `cross` tool is required for Rust cross-compilation but was not found")
	err = phelixerr.Wrapf(phelixerr.CodeToolchainNotFound, err, "cross check failed")
	if _, ok := For(err); ok {
		t.Fatalf("TOOLCHAIN_NOT_FOUND without Go/Rust identity must stay unknown")
	}
}

func TestToolchainResolverNarrowTokens(t *testing.T) {
	stubInstallCommand(t, "cmd-go", "cmd-rust")

	// "cargo build failed" text inside a BUILD_FAILED chain must not match.
	if _, ok := For(buildFail("cargo", "cargo build failed")); ok {
		t.Fatalf("cargo build failure must not produce an install report")
	}
}
