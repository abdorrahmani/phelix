package toolchain

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/abdorrahmani/phelix/internal/builder"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// goVersion is the Go version installed by the official-tarball fallback path.
// It is intentionally pinned so installs are reproducible; bump as needed.
const goVersion = "1.26.3"

// ErrNeedsManualInstall is returned (wrapped with context) when the toolchain
// cannot be installed automatically, e.g. due to missing privileges. Callers
// should surface the manual installation instructions alongside the error.
var ErrNeedsManualInstall = errors.New("automatic installation not possible; please install manually")

// Install installs the toolchain for lang on the current OS.
func Install(lang builder.Language) error {
	switch lang {
	case builder.Go:
		return installGo()
	case builder.Rust:
		return installRust()
	default:
		return phelixerr.Newf(phelixerr.CodeUnsupportedProject, "unsupported language: %s", lang)
	}
}

// ----------------------------- Rust --------------------------------------

func installRust() error {
	switch runtime.GOOS {
	case "windows":
		return installRustWindows()
	default:
		return installRustUnix()
	}
}

// installRustUnix installs Rust via rustup (no privileges required).
func installRustUnix() error {
	if _, err := exec.LookPath("curl"); err != nil {
		return phelixerr.New(phelixerr.CodeToolchainNotFound, "curl is required to install rustup but was not found")
	}
	// sh -c 'curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain stable'
	cmd := exec.Command("sh", "-c",
		"curl --proto =https --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain stable")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "rustup installer failed", err)
	}

	refreshPath()
	return nil
}

// installRustWindows downloads and runs rustup-init.exe.
func installRustWindows() error {
	// Prefer winget if available.
	if _, err := exec.LookPath("winget"); err == nil {
		if err := runCommand("winget", "install", "--id", "Rustlang.Rustup", "-e", "--silent", "--accept-package-agreements", "--accept-source-agreements"); err == nil {
			refreshPath()
			return nil
		}
	}

	tmp, err := os.CreateTemp("", "rustup-init*.exe")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to create temp file", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	_ = tmp.Close()

	if err := downloadFile("https://win.rustup.rs/x86_64", tmpPath); err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "failed to download rustup-init.exe", err)
	}

	if err := runCommand(tmpPath, "-y", "--default-toolchain", "stable"); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "rustup installer failed", err)
	}

	refreshPath()
	return nil
}

// ------------------------------ Go ---------------------------------------

func installGo() error {
	switch runtime.GOOS {
	case "windows":
		return installGoWindows()
	case "darwin":
		// macOS: Homebrew if available, else official tarball.
		if _, err := exec.LookPath("brew"); err == nil {
			if err := runCommand("brew", "install", "go"); err != nil {
				return err
			}
			refreshPath()
			return nil
		}
		return installGoUnixTarball()
	default:
		return installGoLinux()
	}
}

// installGoLinux uses the distro package manager if one is available and
// privilege rules permit it; otherwise it falls back to the official tarball.
func installGoLinux() error {
	if pm, args, ok := detectPackageManager("install", []string{"go", "golang"}); ok {
		// Package managers need root on Linux; use sudo if necessary.
		if err := runPrivileged(pm, args); err != nil {
			return err
		}
		refreshPath()
		return nil
	}
	return installGoUnixTarball()
}

// installGoUnixTarball downloads the official Go tarball and extracts it to
// /usr/local/go. On Linux this requires sudo; on macOS it's tried without.
func installGoUnixTarball() error {
	arch := goArch(runtime.GOARCH)
	if arch == "" {
		return phelixerr.Newf(phelixerr.CodeUnsupportedProject, "unsupported architecture for Go install: %s", runtime.GOARCH)
	}

	osName := runtime.GOOS
	url := fmt.Sprintf("https://go.dev/dl/go%s.%s-%s.tar.gz", goVersion, osName, arch)

	tmpArchive := filepath.Join(os.TempDir(), fmt.Sprintf("go%s.tar.gz", goVersion))
	defer os.Remove(tmpArchive)

	if err := downloadFile(url, tmpArchive); err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "failed to download Go tarball", err)
	}

	target := "/usr/local"
	if runtime.GOOS == "darwin" {
		// On macOS extract to /usr/local as well; may require sudo.
		target = "/usr/local"
	}

	// Remove any previous install then extract. tar handles both GNU and BSD tar.
	extract := func() error {
		// rm -rf $target/go  (ignore error if absent)
		_ = runPrivileged("rm", []string{"-rf", filepath.Join(target, "go")})
		return runPrivileged("tar", []string{"-C", target, "-xzf", tmpArchive})
	}

	if err := extract(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to extract Go tarball", err)
	}

	refreshPath()
	return nil
}

// installGoWindows prefers winget, then falls back to the official MSI.
func installGoWindows() error {
	if _, err := exec.LookPath("winget"); err == nil {
		if err := runCommand("winget", "install", "--id", "GoLang.Go", "-e", "--silent",
			"--accept-package-agreements", "--accept-source-agreements"); err == nil {
			refreshPath()
			return nil
		}
	}

	arch := goArch(runtime.GOARCH)
	if arch == "" {
		return phelixerr.Newf(phelixerr.CodeUnsupportedProject, "unsupported architecture for Go install: %s", runtime.GOARCH)
	}
	msiURL := fmt.Sprintf("https://go.dev/dl/go%s.windows-%s.msi", goVersion, arch)

	tmpMSI := filepath.Join(os.TempDir(), fmt.Sprintf("go%s.msi", goVersion))
	defer os.Remove(tmpMSI)

	if err := downloadFile(msiURL, tmpMSI); err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "failed to download Go MSI", err)
	}
	// msiexec requires elevation; runPrivileged handles Windows via runas.
	if err := runPrivileged("msiexec", []string{"/i", tmpMSI, "/quiet"}); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to install Go MSI", err)
	}
	refreshPath()
	return nil
}

// goArch maps runtime.GOARCH to the Go download identifier.
func goArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	case "386":
		return "386"
	default:
		return ""
	}
}

// ----------------------- privilege handling ------------------------------

// canSudo returns true if sudo is available AND passwordless (already cached).
// We never block waiting on a password prompt from sudo; if a password is
// required we treat it as "no automatic privilege" and instruct the user.
func canSudo() bool {
	if _, err := exec.LookPath("sudo"); err != nil {
		return false
	}
	cmd := exec.Command("sudo", "-n", "true")
	return cmd.Run() == nil
}

// runCommand runs a command with its output connected to this process.
func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runPrivileged runs a command directly. If it fails due to a permission
// error, it retries with sudo (on unix) when canSudo() is true, or via runas
// on Windows. If elevation is unavailable, it returns ErrNeedsManualInstall.
func runPrivileged(name string, args []string) error {
	if runtime.GOOS == "windows" {
		return runWindowsElevated(name, args)
	}

	// First attempt without sudo.
	if err := exec.Command(name, args...).Run(); err == nil {
		return nil
	} else if !isPermissionError(err) {
		// Non-permission failure: still return the error (caller wraps it).
		return err
	}

	// Permission error: retry with sudo if passwordless sudo is available.
	if canSudo() {
		sudoArgs := append([]string{name}, args...)
		cmd := exec.Command("sudo", sudoArgs...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	}

	// No privilege path available.
	return ErrNeedsManualInstall
}

// runWindowsElevated attempts an elevated run via PowerShell Start-Process -Verb RunAs.
// If elevation is denied or unavailable it returns ErrNeedsManualInstall.
func runWindowsElevated(name string, args []string) error {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		return ErrNeedsManualInstall
	}

	// Build: powershell -Command "Start-Process -FilePath 'name' -ArgumentList 'a','b' -Verb RunAs -Wait"
	argList := make([]string, 0, len(args))
	argList = append(argList, "-Command")
	cmdStr := fmt.Sprintf("Start-Process -FilePath '%s' -ArgumentList ", name)
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, fmt.Sprintf("'%s'", strings.ReplaceAll(a, "'", "''")))
	}
	cmdStr += strings.Join(quoted, ",")
	cmdStr += " -Verb RunAs -Wait"
	argList = append(argList, cmdStr)

	cmd := exec.Command(ps, argList...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return ErrNeedsManualInstall
	}
	return nil
}

// isPermissionError returns true if the error is due to insufficient privileges.
func isPermissionError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "permission denied") ||
		strings.Contains(s, "operation not permitted") ||
		strings.Contains(s, "access is denied") ||
		strings.Contains(s, "exit status 1") // many installers return 1 on EACCES
}

// ----------------------------- helpers -----------------------------------

// downloadFile downloads url to dest.
func downloadFile(url, dest string) error {
	resp, err := http.Get(url) //nolint:gosec // URL is constructed from trusted constants
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "failed to download file", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return phelixerr.Newf(phelixerr.CodeNetwork, "unexpected HTTP status %d while downloading", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create download file %s", dest)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return phelixerr.Wrap(phelixerr.CodeNetwork, "failed to write downloaded file", err)
	}
	return nil
}

// refreshPath prepends well-known toolchain bin directories to PATH so the
// freshly installed tool is found by the same process that installed it.
func refreshPath() {
	home, _ := os.UserHomeDir()

	candidates := []string{
		filepath.Join(home, ".cargo", "bin"),
		"/usr/local/go/bin",
		filepath.Join(home, "go", "bin"),
	}

	cur := os.Getenv("PATH")
	seen := map[string]bool{}
	for _, p := range strings.Split(cur, string(os.PathListSeparator)) {
		seen[p] = true
	}

	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			parts = append(parts, c)
		}
	}
	if len(parts) > 0 {
		_ = os.Setenv("PATH", strings.Join(parts, string(os.PathListSeparator))+string(os.PathListSeparator)+cur)
	}
}

// detectPackageManager searches for a known package manager and returns it
// along with the command line to install a package. action is typically
// "install". pkg is the default package name; distros that use a different
// name (e.g. Debian uses "golang") override it.
func detectPackageManager(action string, pkg []string) (string, []string, bool) {
	type pm struct {
		binary string
		args   func(action string, pkg string) []string
		pkg    string // override if the default pkg name differs
	}

	candidates := []pm{
		{"brew", func(a, p string) []string { return []string{a, p} }, pkg[0]},
		{"pacman", func(a, p string) []string { return []string{"-S", "--noconfirm", p} }, pkg[0]},
		{"dnf", func(a, p string) []string { return []string{"-y", a, p} }, pkg[0]},
		{"yum", func(a, p string) []string { return []string{"-y", a, p} }, pkg[0]},
		{"zypper", func(a, p string) []string { return []string{"--non-interactive", a, p} }, pkg[0]},
		{"apk", func(a, p string) []string { return []string{a, "--no-cache"} }, pkg[0]},
		{"apt", func(a, p string) []string { return []string{"-y", a, p} }, "golang"}, // Debian uses "golang"
		{"apt-get", func(a, p string) []string { return []string{"-y", a, p} }, "golang"},
	}

	for _, c := range candidates {
		if _, err := exec.LookPath(c.binary); err == nil {
			return c.binary, c.args(action, c.pkg), true
		}
	}
	return "", nil, false
}
