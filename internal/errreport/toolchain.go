package errreport

import (
	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/toolchain"
)

// InstallCommand resolves the platform-specific install command for a
// language. It defaults to toolchain.InstallCommand (which reuses Phelix's
// existing package-manager detection) and is a variable only so tests can
// substitute a deterministic stub instead of probing the host PATH.
var InstallCommand = toolchain.InstallCommand

// toolchainResolver produces install guidance for TOOLCHAIN_NOT_FOUND errors.
// The affected language is not carried as a typed value on these errors — the
// structured error is all `phelix build` sees — so detection matches the
// structured code plus narrow tool-identity tokens in the wrap-chain text
// ("Go toolchain", "Rust toolchain", "rustup", ...). A TOOLCHAIN_NOT_FOUND
// whose chain names neither toolchain stays unknown.
type toolchainResolver struct{}

func (toolchainResolver) Resolve(err error) (Report, bool) {
	if !phelixerr.IsCode(err, phelixerr.CodeToolchainNotFound) {
		return Report{}, false
	}
	text := chainText(err)
	switch {
	case containsAny(text, "go toolchain", "golang"):
		return toolchainReport(builder.Go), true
	case containsAny(text, "rust toolchain", "cargo", "rustup"):
		return toolchainReport(builder.Rust), true
	default:
		return Report{}, false
	}
}

func toolchainReport(lang builder.Language) Report {
	rep := Report{Code: phelixerr.CodeToolchainNotFound}
	switch lang {
	case builder.Go:
		rep.Title = "Go toolchain not found"
		rep.Explanation = "Phelix could not build this application because the Go compiler (go) is not installed or is not available in PATH."
		rep.Suggestion = "Install Go, then build again. Phelix can also offer to install it interactively on the next run."
		rep.DocsURL = "https://go.dev/doc/install"
		if cmd := InstallCommand(builder.Go); cmd != "" {
			rep.Command = cmd
		} else {
			// No package manager / installer this resolver can name for this
			// platform: point at the official download instead of guessing.
			rep.Suggestion += " Download the installer for your platform from https://go.dev/dl/."
		}
	case builder.Rust:
		rep.Title = "Rust toolchain not found"
		rep.Explanation = "Phelix could not build this application because the Rust toolchain (cargo/rustup) is not installed or is not available in PATH."
		rep.Suggestion = "Install Rust via rustup, then build again. Phelix can also offer to install it interactively on the next run."
		rep.DocsURL = "https://www.rust-lang.org/tools/install"
		if cmd := InstallCommand(builder.Rust); cmd != "" {
			rep.Command = cmd
		} else {
			rep.Suggestion += " Download rustup-init for your platform from https://rustup.rs/."
		}
	}
	return rep
}
