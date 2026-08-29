package update

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/syscmd"
)

var errFakeBoom = errBoom{}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// --- Static release validation -------------------------------------------------

func TestValidateReleaseFileAcceptsRealRelease(t *testing.T) {
	bin := buildPhelixRelease(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "phelix-9.9.9")
	writeFile(t, path, bin, 0o600)

	// The stamped version is accepted with or without the v prefix.
	if err := ValidateReleaseFile(path, releaseTestVersion); err != nil {
		t.Fatalf("ValidateReleaseFile() unexpected error: %v", err)
	}
	if err := ValidateReleaseFile(path, "v"+releaseTestVersion); err != nil {
		t.Fatalf("ValidateReleaseFile(v-prefixed) unexpected error: %v", err)
	}
}

func TestValidateReleaseFileRejectsWrongVersion(t *testing.T) {
	bin := buildPhelixRelease(t)
	path := filepath.Join(t.TempDir(), "phelix")
	writeFile(t, path, bin, 0o600)

	err := ValidateReleaseFile(path, "9.9.8")
	if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
		t.Fatalf("error = %v, want CodeUpdateFailed", err)
	}
	if !strings.Contains(err.Error(), "reports version") {
		t.Fatalf("error should name the reported version: %v", err)
	}
}

func TestValidateReleaseFileRejectsGarbage(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bin")
		writeFile(t, path, nil, 0o755)
		err := ValidateReleaseFile(path, "1.0.0")
		if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
			t.Fatalf("error = %v, want CodeUpdateFailed", err)
		}
	})

	t.Run("text file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bin")
		writeFile(t, path, []byte("this is not a binary"), 0o755)
		err := ValidateReleaseFile(path, "1.0.0")
		if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
			t.Fatalf("error = %v, want CodeUpdateFailed", err)
		}
	})

	t.Run("truncated executable magic", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bin")
		writeFile(t, path, []byte{0x7f, 'E', 'L', 'F'}, 0o755)
		err := ValidateReleaseFile(path, "1.0.0")
		if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
			t.Fatalf("error = %v, want CodeUpdateFailed", err)
		}
	})

	t.Run("directory", func(t *testing.T) {
		err := ValidateReleaseFile(t.TempDir(), "1.0.0")
		if err == nil {
			t.Fatal("expected error for a directory path")
		}
	})
}

// --- Installed-file identity -----------------------------------------------------

func TestVerifyInstalledFile(t *testing.T) {
	bin := buildPhelixRelease(t)
	dir := t.TempDir()
	download := filepath.Join(dir, "download")
	installed := filepath.Join(dir, "installed")
	writeFile(t, download, bin, 0o600)

	writeFile(t, installed, bin, 0o755)
	if err := VerifyInstalledFile(installed, download); err != nil {
		t.Fatalf("VerifyInstalledFile() unexpected error: %v", err)
	}

	// A corrupted replacement must be caught.
	writeFile(t, installed, append([]byte(nil), bin[:len(bin)-1]...), 0o755)
	err := VerifyInstalledFile(installed, download)
	if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
		t.Fatalf("error = %v, want CodeUpdateFailed", err)
	}
}

// --- Atomic replacement ----------------------------------------------------------

func TestReplaceBinaryPlain(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "phelix")
	newBinary := filepath.Join(dir, "new")
	writeFile(t, target, []byte("OLD-BINARY"), 0o755)
	writeFile(t, newBinary, []byte("NEW-BINARY"), 0o600)

	if err := replaceBinary(syscmd.ExecRunner{}, newBinary, target, false); err != nil {
		t.Fatalf("replaceBinary() error: %v", err)
	}
	if got := readFile(t, target); string(got) != "NEW-BINARY" {
		t.Fatalf("target content = %q, want NEW-BINARY", got)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("target mode = %v, want 0755", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".phelix-update-") {
			t.Fatalf("staging file %s was left behind", e.Name())
		}
	}
}

func TestReplaceBinaryElevatedUsesSudoInstallAndMove(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "phelix")
	newBinary := filepath.Join(dir, "new")
	writeFile(t, target, []byte("OLD"), 0o755)
	writeFile(t, newBinary, []byte("NEW"), 0o600)

	stage := dir + "/.phelix-update-" + strconv.Itoa(os.Getpid()) + ".tmp"
	runner := &fakeRunner{responses: map[string][]fakeResp{
		"sudo install -m 0755 " + newBinary + " " + stage: {{}},
		"sudo mv -f " + stage + " " + target:              {{}},
	}}
	if err := replaceBinary(runner, newBinary, target, true); err != nil {
		t.Fatalf("replaceBinary() error: %v", err)
	}
	if !runner.hasCall("sudo install -m 0755 " + newBinary + " " + stage) {
		t.Fatalf("expected sudo install call, got %v", runner.calls)
	}
	if !runner.hasCall("sudo mv -f " + stage + " " + target) {
		t.Fatalf("expected atomic sudo mv call, got %v", runner.calls)
	}
	// The current binary is only replaced through the privileged rename; the
	// fake never touches it, so the old content must still be on disk.
	if got := readFile(t, target); string(got) != "OLD" {
		t.Fatalf("target content = %q, want OLD (fake must do the swap)", got)
	}
}

func TestReplaceBinaryElevatedFailureLeavesStageCleanup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "phelix")
	newBinary := filepath.Join(dir, "new")
	writeFile(t, target, []byte("OLD"), 0o755)
	writeFile(t, newBinary, []byte("NEW"), 0o600)

	stage := dir + "/.phelix-update-" + strconv.Itoa(os.Getpid()) + ".tmp"
	runner := &fakeRunner{responses: map[string][]fakeResp{
		"sudo install -m 0755 " + newBinary + " " + stage: {{}},
		"sudo mv -f " + stage + " " + target:              {{err: errFakeBoom}},
		"sudo rm -f " + stage:                             {{}},
	}}
	err := replaceBinary(runner, newBinary, target, true)
	if !phelixerr.IsCode(err, phelixerr.CodePermissionDenied) {
		t.Fatalf("error = %v, want CodePermissionDenied", err)
	}
	if !runner.hasCall("sudo rm -f " + stage) {
		t.Fatalf("failed elevated replace must clean up the staged file, got %v", runner.calls)
	}
}

// --- Backup -----------------------------------------------------------------------

func TestBackupBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "phelix")
	writeFile(t, target, []byte("CURRENT-BINARY"), 0o755)
	dest := t.TempDir()

	backup, err := backupBinary(target, dest)
	if err != nil {
		t.Fatalf("backupBinary() error: %v", err)
	}
	if got := readFile(t, backup); string(got) != "CURRENT-BINARY" {
		t.Fatalf("backup content = %q", got)
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("backup mode = %v, want 0755 (preserved)", info.Mode().Perm())
	}
}

// --- Permission detection -----------------------------------------------------------

func TestCanWriteTo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are meaningless as root")
	}
	if !canWriteTo(t.TempDir()) {
		t.Fatal("writable dir reported as unwritable")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if canWriteTo(dir) {
		t.Fatal("read-only dir reported as writable")
	}
}

func TestExecutablePathIsAbsoluteAndExists(t *testing.T) {
	path, err := executablePath()
	if err != nil {
		t.Fatalf("executablePath() error: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("executablePath() = %q, want absolute", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("executablePath() %s does not exist: %v", path, err)
	}
}
