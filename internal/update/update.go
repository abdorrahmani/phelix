// Package update implements the `phelix update` self-update flow: resolve
// the latest stable release from the same release server the one-line
// installer uses, download the matching prebuilt binary, verify its SHA-256
// checksum when one is published, validate it statically, atomically replace
// the running executable, and restart the systemd monitor service when one
// was running.
//
// Release contract (shared with install.sh):
//
//	<base>/releases/<version>/phelix-<os>-<arch>
//	<base>/releases/<version>/phelix-<os>-<arch>.sha256   (optional)
//	<base>/releases/latest/version                        (X-Phelix-Version)
package update

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/syscmd"
)

// waitActiveTimeout bounds the post-restart service health wait, matching
// install.sh's 15s window.
const waitActiveTimeout = 15 * time.Second

// StepKind classifies a progress line emitted during an update so the CLI
// can render it with the right symbol/color.
type StepKind int

const (
	StepInfo StepKind = iota
	StepSuccess
	StepWarn
)

// StepFunc receives every progress line. Nil callbacks in Options mean
// silent operation.
type StepFunc func(kind StepKind, format string, args ...any)

// Options configures Run.
type Options struct {
	// BaseURL overrides DefaultBaseURL. Empty means the public release host.
	BaseURL string
	// CurrentVersion is the running binary's version — the same string
	// `phelix version` prints (internal/version.Version).
	CurrentVersion string
	// CheckOnly resolves and compares versions without downloading,
	// replacing, or restarting anything.
	CheckOnly bool
	// StdinIsTTY reports whether sudo may prompt interactively.
	StdinIsTTY bool
	// HTTPClient overrides the release HTTP client (tests).
	HTTPClient *http.Client
	// Runner overrides command execution (tests).
	Runner syscmd.Runner
	// Steps receives progress lines (tests may record them).
	Steps StepFunc
	// TargetPath overrides the binary to replace (tests). Empty means the
	// running executable.
	TargetPath string
	// ServiceUnit overrides the systemd unit name (tests).
	ServiceUnit string
	// ServicePoll overrides the WaitActive polling interval (tests).
	ServicePoll time.Duration
	// WaitActiveTimeout overrides the post-restart health wait (tests).
	WaitActiveTimeout time.Duration
}

// Outcome classifies how Run ended.
type Outcome int

const (
	// OutcomeUpToDate: the installed version equals the latest stable.
	OutcomeUpToDate Outcome = iota
	// OutcomeLocalNewer: the installed version is newer than the latest
	// stable release; the updater never downgrades.
	OutcomeLocalNewer
	// OutcomeAvailable: an update exists (CheckOnly runs only).
	OutcomeAvailable
	// OutcomeUpdated: a new version was installed and verified.
	OutcomeUpdated
)

// Result reports what Run did.
type Result struct {
	Outcome   Outcome
	Current   string // canonical "v1.2.3" display form
	Latest    string // canonical "v1.2.3" display form
	Target    string // binary path that was (or would be) replaced
	Restarted bool   // the systemd service was restarted after replacement
}

// Run executes the update flow. Every failure leaves the previously
// installed binary in place (or restored), and never touches ~/.phelix
// application state.
func Run(opts Options) (*Result, error) {
	steps := opts.Steps
	if steps == nil {
		steps = func(StepKind, string, ...any) {}
	}
	var runner syscmd.Runner = opts.Runner
	if runner == nil {
		runner = syscmd.ExecRunner{}
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client := &ReleaseClient{BaseURL: baseURL, HTTP: opts.HTTPClient}

	steps(StepInfo, "Checking for updates...")

	// Current version — same source as `phelix version`. A development or
	// unknown version carries no release semantics, so it is never treated
	// as "older than everything"; refuse rather than blindly replace.
	curRaw := strings.TrimSpace(opts.CurrentVersion)
	if isUnknownVersion(curRaw) {
		return nil, phelixerr.New(phelixerr.CodeConfiguration,
			"current Phelix version could not be determined (development or unversioned build)")
	}
	cur, err := ParseSemver(curRaw)
	if err != nil {
		return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
			"current Phelix version could not be determined (%q is not a release version)", curRaw)
	}

	latestRaw, err := client.ResolveLatest()
	if err != nil {
		return nil, err
	}
	latest, err := ParseSemver(latestRaw)
	if err != nil {
		return nil, phelixerr.Newf(phelixerr.CodeConfiguration,
			"the release server returned a malformed version (%q)", latestRaw)
	}

	curD, latestD := Display(cur), Display(latest)
	res := &Result{Current: curD, Latest: latestD}

	steps(StepInfo, "Current version: %s", curD)
	steps(StepInfo, "Latest version:  %s", latestD)

	switch cmp := Compare(cur, latest); {
	case cmp > 0:
		// Never downgrade: a locally newer build stays in place.
		steps(StepInfo, "Latest stable release (%s) is older than the installed version (%s); nothing to update.", latestD, curD)
		res.Outcome = OutcomeLocalNewer
		return res, nil
	case cmp == 0:
		steps(StepSuccess, "Phelix is already up to date (%s).", curD)
		res.Outcome = OutcomeUpToDate
		return res, nil
	}
	res.Outcome = OutcomeAvailable
	if opts.CheckOnly {
		steps(StepSuccess, "Update available.")
		return res, nil
	}

	plat, err := DetectPlatform()
	if err != nil {
		return nil, err
	}

	// Download into a temp workspace; the currently installed binary is not
	// touched until the new one has passed every check.
	workDir, err := os.MkdirTemp("", "phelix-update-")
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "could not create a temp workspace", err)
	}
	defer os.RemoveAll(workDir)
	downloadPath := filepath.Join(workDir, "phelix-release.bin")

	steps(StepInfo, "Downloading Phelix %s for %s...", latestD, plat)
	if err := client.DownloadArtifact(latestRaw, plat, downloadPath); err != nil {
		return nil, err
	}

	steps(StepInfo, "Verifying SHA-256 checksum...")
	sumHex, found, err := client.FetchChecksum(latestRaw, plat)
	if err != nil {
		return nil, err
	}
	if found {
		if err := VerifyChecksum(downloadPath, sumHex); err != nil {
			return nil, err
		}
		steps(StepSuccess, "Checksum OK.")
	} else {
		// install.sh's policy: no published checksum → warn and continue.
		steps(StepWarn, "No checksum published for this release; skipping verification.")
	}

	steps(StepInfo, "Validating new binary...")
	if err := ValidateReleaseFile(downloadPath, latestRaw); err != nil {
		return nil, err
	}
	steps(StepSuccess, "Binary validated.")

	target := opts.TargetPath
	if target == "" {
		target, err = executablePath()
		if err != nil {
			return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "could not locate the running Phelix executable", err)
		}
	}
	res.Target = target

	// Plan the systemd restart before mutating anything: only an ACTIVE
	// phelix.service is restarted afterwards (KillMode=process means the
	// restart stops just the monitor, never managed applications). An
	// installed-but-stopped service is left stopped.
	restartPlanned := false
	svc := Systemd{Unit: opts.ServiceUnit, Runner: runner, Poll: opts.ServicePoll}
	if runtime.GOOS == "linux" && svc.Available() && svc.UnitExists() {
		restartPlanned = svc.IsActive()
		if !restartPlanned {
			steps(StepInfo, "%s.service is installed but not running; leaving it stopped.", svc.unit())
		}
	}

	// Privileges: replacing the binary needs write access to the target
	// directory; restarting the system service needs root. Authenticate
	// sudo up front, before anything is modified, so the password prompt
	// happens at a clean point or fails cleanly.
	elevate := !canWriteTo(filepath.Dir(target))
	restartElevated := restartPlanned && os.Geteuid() != 0
	if elevate || restartElevated {
		if err := ensureSudo(runner, opts.StdinIsTTY); err != nil {
			return nil, err
		}
	}

	// Back up the current binary so a failed post-replacement verification
	// or service restart can be rolled back.
	backup, err := backupBinary(target, filepath.Join(workDir, "backup"))
	if err != nil {
		return nil, err
	}

	steps(StepInfo, "Installing update...")
	if err := replaceBinary(runner, downloadPath, target, elevate); err != nil {
		return nil, err
	}

	rollback := func(cause error) error {
		if rbErr := replaceBinary(runner, backup, target, elevate); rbErr != nil {
			return phelixerr.Newf(phelixerr.CodeUpdateFailed,
				"update failed (%v) and restoring the previous binary also failed (%v); %s remains replaced", cause, rbErr, target)
		}
		if restartPlanned {
			_ = svc.Restart(restartElevated)
			return phelixerr.Newf(phelixerr.CodeUpdateFailed,
				"update failed (%v); the previous binary was restored and %s.service restart attempted", cause, svc.unit())
		}
		return phelixerr.Newf(phelixerr.CodeUpdateFailed,
			"update failed (%v); the previous binary was restored", cause)
	}

	// Prove the installed file is exactly the validated release before any
	// service is restarted.
	if err := VerifyInstalledFile(target, downloadPath); err != nil {
		return nil, rollback(err)
	}

	if restartPlanned {
		steps(StepInfo, "Restarting %s.service...", svc.unit())
		if err := svc.Restart(restartElevated); err != nil {
			return nil, rollback(err)
		}
		waitTimeout := opts.WaitActiveTimeout
		if waitTimeout <= 0 {
			waitTimeout = waitActiveTimeout
		}
		if err := svc.WaitActive(waitTimeout); err != nil {
			return nil, rollback(err)
		}
		steps(StepSuccess, "%s.service is active", svc.unit())
		res.Restarted = true
	}

	steps(StepSuccess, "Phelix updated successfully: %s → %s", curD, latestD)
	res.Outcome = OutcomeUpdated
	return res, nil
}

// ensureSudo confirms sudo is usable before any privileged step: cached
// credentials pass immediately; otherwise the user is prompted on their
// terminal when one is attached, and a clear error is returned otherwise.
func ensureSudo(run syscmd.Runner, stdinIsTTY bool) error {
	if _, err := run.Run("sudo", "-n", "true"); err == nil {
		return nil
	}
	if !stdinIsTTY {
		return phelixerr.New(phelixerr.CodePermissionDenied,
			"updating the Phelix binary requires root privileges, but sudo needs a password and no terminal is available")
	}
	if _, err := run.Run("sudo", "-v"); err != nil {
		return phelixerr.Wrap(phelixerr.CodePermissionDenied, "sudo authentication failed", err)
	}
	return nil
}
