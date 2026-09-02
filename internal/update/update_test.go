package update

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// systemdRunner builds a fakeRunner where systemd is installed and (unless
// isActiveStates say otherwise) the phelix unit is active. restartErr makes
// every restart attempt fail. Both the plain and sudo-prefixed restart
// commands are pre-registered so the expected key works for root and
// non-root test users alike.
func systemdRunner(restartErr error, isActiveStates ...fakeResp) *fakeRunner {
	if len(isActiveStates) == 0 {
		isActiveStates = []fakeResp{{out: "active\n"}}
	}
	restart := []fakeResp{{}}
	if restartErr != nil {
		restart = []fakeResp{{err: restartErr}}
	}
	return &fakeRunner{responses: map[string][]fakeResp{
		"systemctl --version":                   {{out: "systemd 255"}},
		"systemctl cat phelix.service":          {{out: "# /etc/systemd/system/phelix.service"}},
		"systemctl is-active phelix.service":    isActiveStates,
		"sudo -n true":                          {{}},
		"systemctl restart phelix.service":      restart,
		"sudo systemctl restart phelix.service": restart,
	}}
}

// missingRunner simulates a host without systemd: every command fails.
func missingRunner() *fakeRunner { return &fakeRunner{} }

func newUpdateTarget(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "phelix")
	writeFile(t, path, []byte("OLD-PHELIX-BINARY"), 0o755)
	return path
}

func baseOpts(baseURL, target string, runner *fakeRunner, steps *stepRecorder) Options {
	return Options{
		BaseURL:    baseURL,
		TargetPath: target,
		Runner:     runner,
		Steps:      steps.record,
		HTTPClient: &http.Client{},
	}
}

// updateOpts is baseOpts for a run updating from v9.9.8 to the served
// release (9.9.9).
func updateOpts(baseURL, target string, runner *fakeRunner, steps *stepRecorder) Options {
	opts := baseOpts(baseURL, target, runner, steps)
	opts.CurrentVersion = "9.9.8"
	return opts
}

func restartKey() string {
	if os.Geteuid() == 0 {
		return "systemctl restart phelix.service"
	}
	return "sudo systemctl restart phelix.service"
}

func TestRunAlreadyUpToDate(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, "")
	target := newUpdateTarget(t)
	steps := &stepRecorder{}

	res, err := Run(Options{
		BaseURL:        rs.srv.URL,
		CurrentVersion: releaseTestVersion,
		TargetPath:     target,
		Runner:         systemdRunner(nil),
		Steps:          steps.record,
		HTTPClient:     &http.Client{},
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Outcome != OutcomeUpToDate {
		t.Fatalf("Outcome = %v, want OutcomeUpToDate", res.Outcome)
	}
	if res.Current != "v"+releaseTestVersion || res.Latest != "v"+releaseTestVersion {
		t.Fatalf("versions = %s/%s", res.Current, res.Latest)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("up-to-date run must not touch the binary")
	}
	if len(rs.requests) != 0 {
		t.Fatalf("up-to-date run downloaded artifacts: %v", rs.requests)
	}
	if !steps.contains("already up to date") {
		t.Fatalf("steps missing up-to-date message: %v", steps.all())
	}
}

func TestRunNeverDowngrades(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, "")
	target := newUpdateTarget(t)

	res, err := Run(Options{
		BaseURL:        rs.srv.URL,
		CurrentVersion: "10.0.0",
		TargetPath:     target,
		Runner:         missingRunner(),
		Steps:          (&stepRecorder{}).record,
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Outcome != OutcomeLocalNewer {
		t.Fatalf("Outcome = %v, want OutcomeLocalNewer", res.Outcome)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("local-newer run must not touch the binary")
	}
	if len(rs.requests) != 0 {
		t.Fatalf("no-downgrade run downloaded artifacts: %v", rs.requests)
	}
}

func TestRunCheckOnlyNeverDownloads(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, "")
	target := newUpdateTarget(t)
	steps := &stepRecorder{}

	res, err := Run(Options{
		BaseURL:        rs.srv.URL,
		CurrentVersion: "9.9.8",
		CheckOnly:      true,
		TargetPath:     target,
		Runner:         missingRunner(),
		Steps:          steps.record,
	})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Outcome != OutcomeAvailable {
		t.Fatalf("Outcome = %v, want OutcomeAvailable", res.Outcome)
	}
	if !steps.contains("Update available.") {
		t.Fatalf("steps missing update-available message: %v", steps.all())
	}
	if len(rs.requests) != 0 {
		t.Fatalf("--check downloaded artifacts: %v", rs.requests)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("--check must not touch the binary")
	}
}

func TestRunFullUpdateWithChecksumAndRestart(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, sha256Hex(t, bin))
	target := newUpdateTarget(t)
	runner := systemdRunner(nil)
	steps := &stepRecorder{}

	res, err := Run(updateOpts(rs.srv.URL, target, runner, steps))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Outcome != OutcomeUpdated {
		t.Fatalf("Outcome = %v, want OutcomeUpdated", res.Outcome)
	}
	if !res.Restarted {
		t.Fatal("Restarted = false, want true (service was active)")
	}
	if res.Target != target {
		t.Fatalf("Target = %q, want %q", res.Target, target)
	}

	if got := readFile(t, target); string(got) != string(bin) {
		t.Fatal("target was not replaced with the release binary")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("target mode = %v, want 0755", info.Mode().Perm())
	}

	for _, want := range []string{
		"Checksum OK.",
		"Binary validated.",
		"Installing update...",
		"Restarting phelix.service...",
		"phelix.service is active",
		"updated successfully",
	} {
		if !steps.contains(want) {
			t.Fatalf("steps missing %q: %v", want, steps.all())
		}
	}
	if !runner.hasCall(restartKey()) {
		t.Fatalf("service was not restarted, calls: %v", runner.calls)
	}
}

func TestRunWithoutPublishedChecksumWarnsAndContinues(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, "")
	target := newUpdateTarget(t)
	steps := &stepRecorder{}

	res, err := Run(updateOpts(rs.srv.URL, target, missingRunner(), steps))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Outcome != OutcomeUpdated {
		t.Fatalf("Outcome = %v, want OutcomeUpdated", res.Outcome)
	}
	if !steps.contains("No checksum published") {
		t.Fatalf("steps missing checksum warning: %v", steps.all())
	}
	if got := readFile(t, target); string(got) != string(bin) {
		t.Fatal("target was not replaced")
	}
}

func TestRunChecksumMismatchLeavesBinaryUntouched(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, sha256Hex(t, []byte("WRONG-PAYLOAD")))
	target := newUpdateTarget(t)
	steps := &stepRecorder{}

	_, err := Run(updateOpts(rs.srv.URL, target, missingRunner(), steps))
	if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
		t.Fatalf("error = %v, want CodeUpdateFailed", err)
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error should be a checksum mismatch: %v", err)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("failed checksum must leave the current binary in place")
	}
	if steps.contains("updated successfully") {
		t.Fatal("must not claim success after a checksum mismatch")
	}
}

func TestRunDownloadFailureLeavesBinaryUntouched(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, "")
	rs.binaryStatus = http.StatusNotFound
	target := newUpdateTarget(t)

	_, err := Run(updateOpts(rs.srv.URL, target, missingRunner(), &stepRecorder{}))
	if !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
		t.Fatalf("error = %v, want CodeNotFound", err)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("failed download must leave the current binary in place")
	}
}

func TestRunRestartFailureRollsBack(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, sha256Hex(t, bin))
	target := newUpdateTarget(t)
	runner := systemdRunner(errors.New("exit status 1: Job for phelix.service failed"))
	steps := &stepRecorder{}

	_, err := Run(updateOpts(rs.srv.URL, target, runner, steps))
	if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
		t.Fatalf("error = %v, want CodeUpdateFailed", err)
	}
	if !strings.Contains(err.Error(), "previous binary was restored") {
		t.Fatalf("error should report the rollback: %v", err)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("failed restart must roll back to the previous binary")
	}
	// The service must have been restarted once for the update and once for
	// the rollback.
	count := 0
	for _, c := range runner.calls {
		if c == restartKey() {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("restart attempted %d times, want 2 (update + rollback): %v", count, runner.calls)
	}
	if steps.contains("updated successfully") {
		t.Fatal("must not claim success after a failed restart")
	}
}

func TestRunServiceNeverBecomesActiveRollsBack(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, sha256Hex(t, bin))
	target := newUpdateTarget(t)
	// First is-active (planning) says active; every later poll says inactive.
	runner := systemdRunner(nil,
		fakeResp{out: "active\n"},
		fakeResp{out: "inactive\n", err: errors.New("exit status 3")},
	)
	steps := &stepRecorder{}

	opts := updateOpts(rs.srv.URL, target, runner, steps)
	opts.ServicePoll = time.Millisecond
	opts.WaitActiveTimeout = 40 * time.Millisecond

	_, err := Run(opts)
	if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
		t.Fatalf("error = %v, want CodeUpdateFailed", err)
	}
	if !strings.Contains(err.Error(), "did not become active") {
		t.Fatalf("error should report the wait timeout: %v", err)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("failed health wait must roll back to the previous binary")
	}
}

func TestRunLeavesStoppedServiceStopped(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, sha256Hex(t, bin))
	target := newUpdateTarget(t)
	runner := systemdRunner(nil, fakeResp{out: "inactive\n", err: errors.New("exit status 3")})
	steps := &stepRecorder{}

	res, err := Run(updateOpts(rs.srv.URL, target, runner, steps))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Restarted {
		t.Fatal("a stopped service must not be started by the update")
	}
	if runner.hasCall(restartKey()) {
		t.Fatalf("stopped service was restarted: %v", runner.calls)
	}
	if got := readFile(t, target); string(got) != string(bin) {
		t.Fatal("binary should still be replaced")
	}
}

func TestRunUnknownCurrentVersionIsRefused(t *testing.T) {
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, "")
	target := newUpdateTarget(t)

	for _, v := range []string{"", "0.0.0-dev"} {
		_, err := Run(Options{
			BaseURL:        rs.srv.URL,
			CurrentVersion: v,
			TargetPath:     target,
			Runner:         missingRunner(),
			Steps:          (&stepRecorder{}).record,
		})
		if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
			t.Fatalf("current %q: error = %v, want CodeConfiguration", v, err)
		}
		if !strings.Contains(err.Error(), "could not be determined") {
			t.Fatalf("current %q: error should explain the refusal: %v", v, err)
		}
		if len(rs.requests) != 0 {
			t.Fatalf("current %q: refused run downloaded artifacts", v)
		}
	}
}

func TestRunMalformedLatestVersion(t *testing.T) {
	rs := newReleaseServer(t, "not-a-version", nil, "")
	target := newUpdateTarget(t)

	_, err := Run(updateOpts(rs.srv.URL, target, missingRunner(), &stepRecorder{}))
	if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
		t.Fatalf("error = %v, want CodeConfiguration", err)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("malformed release info must leave the binary in place")
	}
}

func TestRunUnreachableReleaseServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	target := newUpdateTarget(t)
	_, err := Run(updateOpts(url, target, missingRunner(), &stepRecorder{}))
	if !phelixerr.IsCode(err, phelixerr.CodeConnection) {
		t.Fatalf("error = %v, want CodeConnection", err)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("unreachable server must leave the binary in place")
	}
}

func TestRunElevatedReplacementAuthenticatesSudoFirst(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based elevation is meaningless as root")
	}
	bin := buildPhelixRelease(t)
	rs := newReleaseServer(t, releaseTestVersion, bin, sha256Hex(t, bin))

	dir := t.TempDir()
	target := filepath.Join(dir, "phelix")
	writeFile(t, target, []byte("OLD-PHELIX-BINARY"), 0o755)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restore permissions before TempDir cleanup runs.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// sudo auth fails non-interactively but succeeds interactively; the
	// privileged install/mv are no-ops in the fake, so the post-install
	// identity check fails and the update rolls back through the same
	// privileged path.
	runner := &fakeRunner{responseFn: func(command string, n int) (string, error) {
		switch {
		case command == "sudo -n true":
			return "", errors.New("a password is required")
		case command == "sudo -v":
			return "", nil
		case strings.HasPrefix(command, "sudo install "), strings.HasPrefix(command, "sudo mv "):
			return "", nil
		default:
			return "", errors.New("not found: " + command)
		}
	}}
	steps := &stepRecorder{}

	opts := updateOpts(rs.srv.URL, target, runner, steps)
	opts.StdinIsTTY = true
	_, err := Run(opts)
	if !phelixerr.IsCode(err, phelixerr.CodeUpdateFailed) {
		t.Fatalf("error = %v, want CodeUpdateFailed", err)
	}
	if !strings.Contains(err.Error(), "previous binary was restored") {
		t.Fatalf("error should report the rollback: %v", err)
	}

	sudoCalls := 0
	installCalls := 0
	mvCalls := 0
	for _, c := range runner.calls {
		switch {
		case c == "sudo -n true" || c == "sudo -v":
			sudoCalls++
		case strings.HasPrefix(c, "sudo install "):
			installCalls++
		case strings.HasPrefix(c, "sudo mv "):
			mvCalls++
		}
	}
	if sudoCalls < 2 {
		t.Fatalf("sudo must be authenticated before any mutation, calls: %v", runner.calls)
	}
	if installCalls != 2 || mvCalls != 2 {
		t.Fatalf("elevated install+rollback expected 2 install/2 mv calls, got %d/%d: %v",
			installCalls, mvCalls, runner.calls)
	}
	if got := readFile(t, target); string(got) != "OLD-PHELIX-BINARY" {
		t.Fatal("the fake never swapped the binary; it must remain the old one")
	}
}
