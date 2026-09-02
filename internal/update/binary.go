package update

import (
	"bytes"
	"crypto/subtle"
	"debug/buildinfo"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/syscmd"
)

// modulePath is the Go module every genuine Phelix binary reports in its
// embedded build info.
const modulePath = "github.com/abdorrahmani/phelix"

// ldflagsVersionRe extracts the release version a binary was stamped with
// via -ldflags "-X <pkg>.Version=<value>" (how release.sh builds releases).
var ldflagsVersionRe = regexp.MustCompile(`-X\s+"?` + regexp.QuoteMeta(modulePath) + `/internal/version\.Version"?=(\S+)`)

// ValidateReleaseFile statically proves the downloaded artifact is a Phelix
// release binary for this machine, WITHOUT executing it: the file must carry
// a known executable-format magic, parse as a Go binary built from the
// Phelix module, target the running platform, and — when the release was
// stamped with one — carry exactly the expected version.
//
// Executing untrusted download content is deliberately avoided; the
// installed file is later proven byte-identical to this validated file
// (VerifyInstalledFile), so `phelix version` reports the verified version by
// construction.
func ValidateReleaseFile(path, expectedVersion string) error {
	info, err := os.Stat(path)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "inspect downloaded file %s", path)
	}
	if !info.Mode().IsRegular() {
		return phelixerr.Newf(phelixerr.CodeUpdateFailed, "downloaded file %s is not a regular file", path)
	}
	if info.Size() == 0 {
		return phelixerr.New(phelixerr.CodeUpdateFailed, "the downloaded release binary is empty")
	}
	if err := checkExecutableMagic(path); err != nil {
		return err
	}

	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return phelixerr.Newf(phelixerr.CodeUpdateFailed,
			"the downloaded file is not a Go-built Phelix binary (%v)", err)
	}
	if bi.Main.Path != modulePath {
		return phelixerr.Newf(phelixerr.CodeUpdateFailed,
			"the downloaded file was built from module %q, not %q", bi.Main.Path, modulePath)
	}
	if err := checkBuildPlatform(bi); err != nil {
		return err
	}
	if stamped := extractStampedVersion(bi); stamped != "" {
		got, err := ParseSemver(stamped)
		if err != nil {
			return phelixerr.Newf(phelixerr.CodeUpdateFailed,
				"the downloaded binary carries a malformed stamped version %q", stamped)
		}
		want, err := ParseSemver(expectedVersion)
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "target release version is malformed")
		}
		if Compare(got, want) != 0 {
			return phelixerr.Newf(phelixerr.CodeUpdateFailed,
				"the downloaded binary reports version %s, expected %s", Display(got), Display(want))
		}
	}
	return nil
}

// checkExecutableMagic requires one of the executable-format magics the
// supported release platforms produce (ELF for Linux, Mach-O for macOS).
func checkExecutableMagic(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "open %s", path)
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return phelixerr.New(phelixerr.CodeUpdateFailed, "the downloaded file is too short to be a binary")
	}
	for _, magic := range [][]byte{
		{0x7f, 'E', 'L', 'F'},    // ELF (Linux)
		{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O 64-bit little-endian (macOS)
		{0xfe, 0xed, 0xfa, 0xcf}, // Mach-O 64-bit big-endian
		{0xce, 0xfa, 0xed, 0xfe}, // Mach-O 32-bit little-endian
		{0xfe, 0xed, 0xfa, 0xce}, // Mach-O 32-bit big-endian
		{0xca, 0xfe, 0xba, 0xbe}, // Mach-O universal (fat) binary
	} {
		if bytes.Equal(head, magic) {
			return nil
		}
	}
	return phelixerr.New(phelixerr.CodeUpdateFailed,
		"the downloaded file is not a Linux/macOS executable (unrecognized format)")
}

// checkBuildPlatform fails the update when the artifact was built for a
// different OS/architecture than the running one.
func checkBuildPlatform(bi *buildinfo.BuildInfo) error {
	var goos, goarch string
	for _, s := range bi.Settings {
		switch s.Key {
		case "GOOS":
			goos = s.Value
		case "GOARCH":
			goarch = s.Value
		}
	}
	if goos != "" && goos != runtime.GOOS {
		return phelixerr.Newf(phelixerr.CodeUpdateFailed,
			"the downloaded binary targets %s, but this machine is %s", goos, runtime.GOOS)
	}
	if goarch != "" && goarch != runtime.GOARCH {
		return phelixerr.Newf(phelixerr.CodeUpdateFailed,
			"the downloaded binary targets %s, but this machine is %s", goarch, runtime.GOARCH)
	}
	return nil
}

// extractStampedVersion returns the release version embedded at build time,
// or "" when the release was not stamped (the post-install identity check
// still binds the installed file to the checksum-verified download).
func extractStampedVersion(bi *buildinfo.BuildInfo) string {
	for _, s := range bi.Settings {
		if s.Key == "-ldflags" {
			if m := ldflagsVersionRe.FindStringSubmatch(s.Value); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// VerifyInstalledFile proves the file now at installedPath is byte-identical
// to the checksum-verified download at downloadPath. Together with
// ValidateReleaseFile this establishes, without executing anything, that the
// installed binary is the exact validated release — so `phelix version`
// reports the verified version by construction.
func VerifyInstalledFile(installedPath, downloadPath string) error {
	got, err := fileSHA256(installedPath)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hash the installed binary %s", installedPath)
	}
	want, err := fileSHA256(downloadPath)
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "hash the downloaded release %s", downloadPath)
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return phelixerr.Newf(phelixerr.CodeUpdateFailed,
			"the installed binary does not match the validated release (got %s, want %s)", got, want)
	}
	return nil
}

// executablePath locates the running Phelix binary: the path `phelix update`
// must replace. Symlinks are resolved so the real file is swapped in place
// rather than the link.
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		// Same fallback convention as cmd/proxy.go and cmd/health.go.
		exe = os.Args[0]
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// canWriteTo reports whether the current user can create files in dir — the
// permission rename(2) actually needs to replace a file inside it. This keeps
// the updater from reaching for sudo when Phelix was installed somewhere the
// user can already write (e.g. ~/bin).
func canWriteTo(dir string) bool {
	f, err := os.CreateTemp(dir, ".phelix-perm-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// backupBinary copies the current target into destDir (always a
// user-writable temp dir) so a failed post-replacement verification or
// service restart can be rolled back. Returns the backup path.
func backupBinary(target, destDir string) (string, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "could not create the backup directory", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "inspect the current binary %s", target)
	}
	backup := filepath.Join(destDir, "phelix.previous")
	if err := copyFile(target, backup, info.Mode().Perm()); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "could not back up the current binary", err)
	}
	return backup, nil
}

// replaceBinary atomically moves newBinary over target. On Linux and macOS
// rename(2) replaces the file even while a running process is executing it,
// so no helper process is required. The new binary is staged under a temp
// name in the target's directory (same filesystem → atomic rename) and only
// then renamed; a failure at any point leaves the current binary untouched.
//
// When elevate is true the staging and rename run through sudo, because the
// target directory (typically /usr/local/bin) is not writable by the current
// user.
func replaceBinary(run syscmd.Runner, newBinary, target string, elevate bool) error {
	stage := filepath.Join(filepath.Dir(target), fmt.Sprintf(".phelix-update-%d.tmp", os.Getpid()))
	_ = os.Remove(stage)

	if elevate {
		if _, err := run.Run("sudo", "install", "-m", "0755", newBinary, stage); err != nil {
			return phelixerr.Wrapf(phelixerr.CodePermissionDenied, err,
				"could not stage the new binary next to %s (sudo)", target)
		}
		if _, err := run.Run("sudo", "mv", "-f", stage, target); err != nil {
			_, _ = run.Run("sudo", "rm", "-f", stage)
			return phelixerr.Wrapf(phelixerr.CodePermissionDenied, err,
				"could not replace %s (sudo)", target)
		}
		return nil
	}

	if err := copyFile(newBinary, stage, 0o755); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "stage the new binary at %s", stage)
	}
	if err := os.Rename(stage, target); err != nil {
		_ = os.Remove(stage)
		return phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "atomically replace %s", target)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
