package toolchain

import (
	"os/exec"
	"testing"

	"github.com/abdorrahmani/phelix/internal/builder"
)

// stubLookPath replaces the PATH probe with a fake and returns a restore func.
func stubLookPath(t *testing.T, present map[string]bool) {
	t.Helper()
	old := lookPath
	lookPath = func(name string) (string, error) {
		if present[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { lookPath = old })
}

func TestInstallCommandGoLinux(t *testing.T) {
	cases := []struct {
		name    string
		present map[string]bool
		want    string
	}{
		{"apt", map[string]bool{"apt": true}, "sudo apt-get install -y golang"},
		{"apt-get", map[string]bool{"apt-get": true}, "sudo apt-get install -y golang"},
		{"dnf", map[string]bool{"dnf": true}, "sudo dnf install -y go"},
		{"pacman", map[string]bool{"pacman": true}, "sudo pacman -S --noconfirm go"},
		{"apk", map[string]bool{"apk": true}, "sudo apk add --no-cache go"},
		{"brew on linux needs no sudo", map[string]bool{"brew": true}, "brew install go"},
		{"no package manager", map[string]bool{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubLookPath(t, tc.present)
			if got := installCommandFor("linux", builder.Go); got != tc.want {
				t.Fatalf("installCommandFor(linux, go) = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstallCommandGoDarwin(t *testing.T) {
	stubLookPath(t, map[string]bool{"brew": true})
	if got := installCommandFor("darwin", builder.Go); got != "brew install go" {
		t.Fatalf("darwin+brew: got %q", got)
	}

	stubLookPath(t, map[string]bool{})
	if got := installCommandFor("darwin", builder.Go); got != "" {
		t.Fatalf("darwin without brew should suggest the official installer (empty command), got %q", got)
	}
}

func TestInstallCommandGoWindows(t *testing.T) {
	stubLookPath(t, map[string]bool{"winget": true})
	if got := installCommandFor("windows", builder.Go); got != "winget install --id GoLang.Go -e" {
		t.Fatalf("windows+winget: got %q", got)
	}
	stubLookPath(t, map[string]bool{})
	if got := installCommandFor("windows", builder.Go); got != "" {
		t.Fatalf("windows without winget should be empty, got %q", got)
	}
}

func TestInstallCommandRust(t *testing.T) {
	// Rust installs via rustup on unix, regardless of package managers.
	stubLookPath(t, map[string]bool{})
	if got := installCommandFor("linux", builder.Rust); got != "curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh" {
		t.Fatalf("linux rust: got %q", got)
	}
	if got := installCommandFor("darwin", builder.Rust); got != "curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh" {
		t.Fatalf("darwin rust: got %q", got)
	}

	stubLookPath(t, map[string]bool{"winget": true})
	if got := installCommandFor("windows", builder.Rust); got != "winget install --id Rustlang.Rustup -e" {
		t.Fatalf("windows+winget rust: got %q", got)
	}
	stubLookPath(t, map[string]bool{})
	if got := installCommandFor("windows", builder.Rust); got != "" {
		t.Fatalf("windows without winget rust should be empty, got %q", got)
	}
}

func TestInstallCommandUnknownLanguage(t *testing.T) {
	stubLookPath(t, map[string]bool{"apt": true})
	if got := installCommandFor("linux", builder.Language("zig")); got != "" {
		t.Fatalf("unknown language should have no command, got %q", got)
	}
}

func TestInstallCommandSuggestionsAreSafe(t *testing.T) {
	// Every suggested command must be a fixed, reviewable string — never
	// derived from error output. Pin the exact strings here so a regression
	// that starts interpolating dynamic content is caught.
	stubLookPath(t, map[string]bool{"apt": true})
	got := installCommandFor("linux", builder.Go)
	for _, want := range []string{"sudo apt-get install -y golang"} {
		if got != want {
			t.Fatalf("unexpected suggestion %q, want %q", got, want)
		}
	}
}
