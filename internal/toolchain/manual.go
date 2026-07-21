package toolchain

import (
	"runtime"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// ManualInstructions returns OS-specific, copy-paste-ready instructions for
// manually installing the toolchain for the given language. It is printed when
// the user declines the install prompt or when automatic installation fails.
func ManualInstructions(lang builder.Language) string {
	switch lang {
	case builder.Go:
		return manualGo()
	case builder.Rust:
		return manualRust()
	default:
		return "Please install the required toolchain manually."
	}
}

// manualGo returns manual install instructions for Go on the current OS.
func manualGo() string {
	switch runtime.GOOS {
	case "darwin":
		return "Please install Go manually:\n" +
			"  brew install go\n" +
			"  # or download the installer from https://go.dev/dl/\n"
	case "linux":
		return "Please install Go manually:\n" +
			"  # Debian/Ubuntu:         sudo apt-get update && sudo apt-get install -y golang\n" +
			"  # Fedora/RHEL:           sudo dnf install -y go\n" +
			"  # Arch/Manjaro:          sudo pacman -S --noconfirm go\n" +
			"  # Alpine:                sudo apk add --no-cache go\n" +
			"  # openSUSE:              sudo zypper install -y go\n" +
			"  # Or the official tarball:\n" +
			"    sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go" + goVersion + ".linux-*.tar.gz\n" +
			"  Download from: https://go.dev/dl/\n"
	case "windows":
		return "Please install Go manually:\n" +
			"  winget install GoLang.Go\n" +
			"  # or download the MSI installer from https://go.dev/dl/\n"
	default:
		return "Please install Go from https://go.dev/dl/\n"
	}
}

// manualRust returns manual install instructions for Rust on the current OS.
func manualRust() string {
	switch runtime.GOOS {
	case "windows":
		return "Please install Rust manually:\n" +
			"  winget install Rustlang.Rustup\n" +
			"  # or download rustup-init.exe from https://rustup.rs/\n"
	default:
		return "Please install Rust manually via rustup:\n" +
			"  curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh\n" +
			"  # or see https://rustup.rs/\n"
	}
}
