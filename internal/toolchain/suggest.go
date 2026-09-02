package toolchain

import (
	"os/exec"
	"runtime"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// lookPath is the PATH probe used by the suggestion logic below. It is a var
// so tests can stub it instead of depending on the host's installed tools.
var lookPath = exec.LookPath

// InstallCommand returns one concrete, copy-paste-ready command to install
// the toolchain for lang on the current platform. It mirrors Phelix's
// existing package-manager detection (detectPackageManager in installer.go:
// same candidates, same order, same package names) rather than inventing a
// second platform layer, so the suggested command matches what Phelix itself
// would run when asked to install the toolchain automatically.
//
// It returns "" when no deterministic single command can be offered for the
// platform; callers should then point the user at the official download page
// instead of guessing a command. The returned command is only ever a
// suggestion — Phelix never executes it on the user's behalf from the error
// reporter.
func InstallCommand(lang builder.Language) string {
	return installCommandFor(runtime.GOOS, lang)
}

// installCommandFor is InstallCommand with the OS injected so tests can cover
// every platform without depending on the host.
func installCommandFor(goos string, lang builder.Language) string {
	switch lang {
	case builder.Go:
		return goInstallCommand(goos)
	case builder.Rust:
		return rustInstallCommand(goos)
	default:
		return ""
	}
}

// goInstallCommand returns the Go install command for goos, or "" when the
// platform has no single deterministic command (official installer/tarball).
func goInstallCommand(goos string) string {
	switch goos {
	case "darwin":
		if _, err := lookPath("brew"); err == nil {
			return "brew install go"
		}
		return ""
	case "windows":
		if _, err := lookPath("winget"); err == nil {
			return "winget install --id GoLang.Go -e"
		}
		return ""
	case "linux":
		// Mirrors detectPackageManager's candidate order. apk is constructed
		// explicitly (apk's install verb is "add") because the installer's
		// shared table currently omits the package name for apk.
		candidates := []struct {
			binary string
			cmd    string
		}{
			{"brew", "brew install go"},
			{"pacman", "sudo pacman -S --noconfirm go"},
			{"dnf", "sudo dnf install -y go"},
			{"yum", "sudo yum install -y go"},
			{"zypper", "sudo zypper install -y go"},
			{"apk", "sudo apk add --no-cache go"},
			{"apt", "sudo apt-get install -y golang"},
			{"apt-get", "sudo apt-get install -y golang"},
		}
		for _, c := range candidates {
			if _, err := lookPath(c.binary); err == nil {
				return c.cmd
			}
		}
		return ""
	default:
		return ""
	}
}

// rustInstallCommand returns the Rust install command for goos. Rust is
// installed via rustup everywhere except Windows, where winget is preferred.
func rustInstallCommand(goos string) string {
	switch goos {
	case "windows":
		if _, err := lookPath("winget"); err == nil {
			return "winget install --id Rustlang.Rustup -e"
		}
		return ""
	default:
		return "curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh"
	}
}
