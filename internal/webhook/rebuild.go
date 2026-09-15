package webhook

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/monitor"
)

const (
	// deployLockPollInterval is how often a waiting webhook job re-checks the
	// per-app deploy lock.
	deployLockPollInterval = 2 * time.Second

	// maxRebuildAttempts bounds retries after a lost lock race. The window
	// between "lock observed free" and "subprocess acquired it" is tiny, so a
	// couple of retries cover it; anything beyond that is a genuine failure.
	maxRebuildAttempts = 3

	// stderrTailLines is how many trailing stderr lines are kept to classify
	// and report a failed rebuild.
	stderrTailLines = 30
)

// deployLockFailureMarker is the message the rebuild command wraps lock
// contention in (cmd/rebuild.go, both the classic and zero-downtime paths).
const deployLockFailureMarker = "could not acquire deploy lock"

// The rebuild command's documented exit codes (README § Exit codes / cmd/
// cli_errors.go). The webhook layer maps them without importing the cmd
// package.
const (
	rebuildExitBuild       = 20
	rebuildExitDeploy      = 21
	rebuildExitRollback    = 22
	rebuildExitRollbackVer = 23
	rebuildExitAutoRoll    = 24
)

// renderedCodePattern extracts the structured error code the CLI renders into
// stderr ("  Code: BUILD_FAILED"). Classifying from the CLI's own error
// rendering is structured output, not log-text guessing.
var renderedCodePattern = regexp.MustCompile(`Code: ([A-Z][A-Z0-9_]+)`)

// ErrShutdownSubprocess marks the specific shutdown case where the rebuild
// SUBPROCESS is still running as an orphan. Unlike a plain shutdown while
// waiting, the isolated source must stay in place until the orphan finishes —
// it is removed later by the startup sweep, never underneath the running
// build — and the job record stays non-terminal so restart recovery can
// resolve the outcome from the deployment state.
var ErrShutdownSubprocess = errors.New("webhook: shutdown with rebuild subprocess still running")

// commandRunner executes one `phelix rebuild <appID>` invocation in dir and
// returns the trailing stderr lines for failure classification. The job is
// passed so the production runner can advance the durable job record's
// stage from the pipeline's progress output. A field on CliRebuild (not a
// package var) so tests can assert which invocation a job routes to without
// spawning a process.
type commandRunner func(ctx context.Context, job *Job, dir string, args ...string) (stderrTail []string, err error)

// newProductionRunner returns the commandRunner that re-invokes the phelix
// binary's rebuild command in the application's project directory — exactly
// how the monitor daemon's remote rebuild command does it — so the webhook
// job runs through the full existing pipeline (strategy selection from
// phelix.yaml, build, health verification, versioning, auto-rollback and the
// deploy lock all stay inside `phelix rebuild`). Progress lines in the
// subprocess output advance the durable job record's stage.
func newProductionRunner(jobs *JobStore) commandRunner {
	return func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
		cmd, err := monitor.NewPhelixCommand(dir, args...)
		if err != nil {
			return nil, err
		}
		tail := newLineTail(stderrTailLines)
		stages := newStageDetector(job, jobs)
		cmd.Stdout = io.MultiWriter(newLineLogger("webhook", "rebuild stdout:"), stages)
		cmd.Stderr = io.MultiWriter(newLineLogger("webhook", "rebuild stderr:"), tail)

		if err := cmd.Start(); err != nil {
			return tail.snapshot(), phelixerr.Wrap(phelixerr.CodeProcessFailed, "webhook: start rebuild command", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				return tail.snapshot(), phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "webhook: rebuild command failed")
			}
			return tail.snapshot(), nil
		case <-ctx.Done():
			// The rebuild subprocess is an independent CLI invocation, not a
			// child the webhook owns: leave it running so the deploy finishes
			// cleanly (it releases the deploy lock and records versions on
			// its own). The wait below only reaps the process, it does not
			// cancel it. The isolated source it builds from must outlive this
			// job — the startup sweep removes it once it is stale, and the
			// job record stays non-terminal for restart recovery.
			go func() { _ = cmd.Wait() }()
			return tail.snapshot(), phelixerr.Wrap(phelixerr.CodeUnavailable,
				"webhook: server shutdown; rebuild left running to completion",
				errors.Join(ErrShutdown, ErrShutdownSubprocess))
		}
	}
}

// stageMarkers maps stable progress lines from the rebuild pipeline's own
// output onto the durable job record's running status and stage. Best-effort
// progress information only — outcomes never come from output text.
var stageMarkers = []struct {
	match  string
	status string
	stage  string
}{
	{"starting new instance", StatusDeploying, StageDeploy},        // blue-green
	{"starting canary instance", StatusDeploying, StageDeploy},     // rollout
	{"starting replacement", StatusDeploying, StageDeploy},         // rolling
	{"starting first instance", StatusDeploying, StageDeploy},      // rolling
	{"Starting application on port", StatusDeploying, StageDeploy}, // classic
	{"health tier selected", StatusHealthChecking, StageHealthCheck},
}

// stageDetector watches the rebuild subprocess's stdout and advances the
// job record as the pipeline reaches the deploy and health-check phases.
type stageDetector struct {
	job  *Job
	jobs *JobStore
	seen map[string]bool
}

func newStageDetector(job *Job, jobs *JobStore) *stageDetector {
	return &stageDetector{job: job, jobs: jobs, seen: make(map[string]bool)}
}

func (d *stageDetector) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line != "" && d.jobs != nil && d.job != nil && d.job.ID != "" {
		for _, m := range stageMarkers {
			if d.seen[m.stage] {
				continue
			}
			if strings.Contains(line, m.match) {
				d.seen[m.stage] = true
				if err := d.jobs.MarkProgress(d.job.ID, m.status, m.stage); err != nil {
					logs.Debug("webhook", "job %s: stage update to %s: %v", d.job.ID, m.stage, err)
				} else {
					logs.Debug("webhook", "job %s: stage %s", d.job.ID, m.stage)
				}
			}
		}
	}
	return len(p), nil
}

// CliRebuild executes queued webhook jobs through the existing `phelix
// rebuild` pipeline. It is the ONLY bridge between the webhook layer and the
// deployment machinery, and it deliberately contains none of that machinery:
// no build steps, no deployment strategies, no health checks, no version or
// rollback writes, and no deploy lock acquisition of its own. The per-app
// deploy lock stays authoritative inside the rebuild command; a webhook job
// whose app is locked by a manual rebuild or rollback WAITS — it never
// steals, bypasses or force-releases the lock.
//
// The job's exact-commit source is fetched from the app's Git remote and
// checked out into an isolated worktree, which the rebuild consumes via
// --source-dir. A Git synchronization failure fails the job BEFORE any
// rebuild runs — Phelix never falls back to building whatever is currently
// on disk. The durable job record is updated through every lifecycle step;
// a job-record write failure is logged and never blocks the deployment.
type CliRebuild struct {
	poll    time.Duration
	run     commandRunner
	prepare SourcePreparer
	jobs    *JobStore
}

// NewCliRebuild returns the production rebuild service. worktreeRoot is the
// directory holding the temporary isolated sources (and swept at startup);
// jobs is the durable job store (nil disables record updates).
func NewCliRebuild(worktreeRoot string, jobs *JobStore) *CliRebuild {
	return &CliRebuild{
		poll:    deployLockPollInterval,
		run:     newProductionRunner(jobs),
		prepare: NewGitSourcePreparer(worktreeRoot),
		jobs:    jobs,
	}
}

// jobID is the empty-string-safe guard for record updates.
func jobRecordID(job *Job) string {
	if job == nil {
		return ""
	}
	return job.ID
}

// markJob applies a store transition for the job, best-effort: failures are
// logged, never propagated — the deployment itself must not fail because its
// status record could not be written (only CREATE is load-bearing, at
// acceptance time).
func (c *CliRebuild) markJob(job *Job, what string, fn func(id string) error) {
	if c.jobs == nil {
		return
	}
	id := jobRecordID(job)
	if id == "" {
		return
	}
	if err := fn(id); err != nil {
		logs.Warning("webhook", "job %s: could not record %s: %v", id, what, err)
	}
}

// Rebuild prepares the isolated exact-commit source, waits for the app's
// deploy lock to be free, then runs the rebuild command against that source.
// On completion the job record is correlated with the Phelix version the
// pipeline created (matched by the exact pushed commit) and with any
// automatic rollback the pipeline performed.
func (c *CliRebuild) Rebuild(ctx context.Context, job *Job) error {
	if c.prepare == nil {
		return phelixerr.New(phelixerr.CodeServer, "webhook: rebuild service has no source preparer")
	}

	c.markJob(job, "syncing start", func(id string) error { return c.jobs.MarkSyncing(id) })
	logs.Info("webhook", "preparing exact-commit source app=%s job=%s delivery=%s commit=%s branch=%s",
		job.AppName, jobRecordID(job), job.DeliveryID, job.CommitSHA, job.Branch)
	prepared, err := c.prepare(ctx, job)
	if err != nil {
		// Git sync failure: the job fails and no rebuild ever runs — the
		// deployed source must never silently become the on-disk tree.
		logs.Error("webhook", "git source preparation failed app=%s job=%s delivery=%s commit=%s: %v",
			job.AppName, jobRecordID(job), job.DeliveryID, job.CommitSHA, err)
		c.markJob(job, "git sync failure", func(id string) error {
			return c.jobs.MarkFailed(id, JobErrGitSync, err.Error())
		})
		return err
	}

	orphanRunning := false
	defer func() {
		c.markJob(job, "cleanup stage", func(id string) error { return c.jobs.MarkCleanup(id) })
		if orphanRunning {
			logs.Warning("webhook",
				"app=%s job=%s delivery=%s: rebuild left running at shutdown; isolated source %s stays until the startup sweep and the job record is resolved by restart recovery",
				job.AppName, jobRecordID(job), job.DeliveryID, prepared.Dir)
			return
		}
		if cerr := prepared.Cleanup(); cerr != nil {
			// Logged, never returned: cleanup must not mask the job's own
			// outcome.
			logs.Warning("webhook", "app=%s job=%s delivery=%s: isolated source cleanup failed (deployment result unaffected): %v",
				job.AppName, jobRecordID(job), job.DeliveryID, cerr)
		}
	}()

	// Baselines for post-run correlation. While our rebuild runs, no other
	// deploy or rollback for this app can run (it holds the deploy lock), so
	// any NEW rollback-history entry afterwards was written by our rebuild's
	// automatic rollback, and the app's current version is what our rebuild
	// promoted.
	rollbackBaseline := rollbackHistoryLen(job.AppName)

	c.markJob(job, "build start", func(id string) error { return c.jobs.MarkBuilding(id) })
	for attempt := 1; ; attempt++ {
		if err := c.waitForDeployLock(ctx, job); err != nil {
			return err
		}
		stderrTail, err := c.run(ctx, job, job.Directory, "rebuild", job.AppID, "--source-dir", prepared.Dir)
		if err == nil {
			version := correlateDeployedVersion(job)
			c.markJob(job, "success", func(id string) error { return c.jobs.MarkSucceeded(id, version) })
			if version > 0 {
				logs.Info("webhook", "job %s succeeded: app=%s commit=%s deployed as v%d",
					jobRecordID(job), job.AppName, job.CommitSHA, version)
			} else {
				logs.Info("webhook", "job %s succeeded: app=%s commit=%s (no matching version promoted)",
					jobRecordID(job), job.AppName, job.CommitSHA)
			}
			return nil
		}
		if errors.Is(err, ErrShutdownSubprocess) {
			orphanRunning = true
			return err
		}
		if IsShutdownErr(err) {
			return err
		}
		if c.lockContention(job, stderrTail) && attempt < maxRebuildAttempts {
			logs.Info("webhook", "app=%s job=%s rebuild lost the deploy lock race; waiting and retrying (attempt %d/%d)",
				job.AppName, jobRecordID(job), attempt, maxRebuildAttempts)
			continue
		}

		c.markTerminalFailure(job, stderrTail, err, rollbackBaseline)
		return err
	}
}

// markTerminalFailure classifies a failed rebuild and closes the job record.
// The classification uses the structured signals of the existing pipeline:
// the CLI's rendered error code in stderr, the documented exit code, and —
// for automatic rollback — a new entry in the app's rollback history.
func (c *CliRebuild) markTerminalFailure(job *Job, stderrTail []string, runErr error, rollbackBaseline int) {
	errorCode := classifyRebuildFailure(stderrTail, runErr)
	message := failureMessage(stderrTail, runErr)

	if rb := detectAutomaticRollback(job.AppName, rollbackBaseline); rb.detected {
		c.markJob(job, "rolled back", func(id string) error {
			return c.jobs.MarkRolledBack(id, rb.restoredVersion, errorCode, message)
		})
		logs.Error("webhook", "job %s failed and rolled back: app=%s commit=%s restored v%d: %v",
			jobRecordID(job), job.AppName, job.CommitSHA, rb.restoredVersion, runErr)
		return
	}
	c.markJob(job, "failure", func(id string) error {
		return c.jobs.MarkFailed(id, errorCode, message)
	})
	logs.Error("webhook", "job %s failed (%s): app=%s commit=%s: %v",
		jobRecordID(job), errorCode, job.AppName, job.CommitSHA, runErr)
}

// classifyRebuildFailure maps a failed rebuild onto a job error code. The
// CLI renders its structured error code into stderr ("  Code: X"); that is
// preferred. The documented exit code (a stable contract) is the fallback.
func classifyRebuildFailure(stderrTail []string, runErr error) string {
	var matches []string
	for _, line := range stderrTail {
		if m := renderedCodePattern.FindStringSubmatch(line); m != nil {
			matches = append(matches, m[1])
		}
	}
	if len(matches) > 0 {
		switch code := matches[len(matches)-1]; code {
		case "BUILD_FAILED", "TOOLCHAIN_NOT_FOUND", "BUILD_TIMEOUT", "UNSUPPORTED_PROJECT":
			return JobErrBuild
		case "HEALTH_CHECK_FAILED":
			return JobErrHealth
		case "DEPLOY_FAILED", "INSTANCE_START_FAILED", "CANARY_REGRESSION":
			return JobErrDeploy
		case "AUTO_ROLLBACK_FAILED":
			return JobErrAutoRoll
		case "ROLLBACK_FAILED", "ROLLBACK_VERIFY_FAILED":
			return JobErrRollback
		}
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		switch exitErr.ExitCode() {
		case rebuildExitBuild:
			return JobErrBuild
		case rebuildExitDeploy:
			return JobErrDeploy
		case rebuildExitAutoRoll:
			return JobErrAutoRoll
		case rebuildExitRollback, rebuildExitRollbackVer:
			return JobErrRollback
		}
	}
	return JobErrRebuild
}

// failureMessage builds a short, sanitized message from the rebuild's own
// error output.
func failureMessage(stderrTail []string, runErr error) string {
	for i := len(stderrTail) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(stderrTail[i]); line != "" && !strings.HasPrefix(line, "Code:") &&
			!strings.HasPrefix(line, "Hint:") && !strings.HasPrefix(line, "Run with --debug") {
			return line
		}
	}
	if runErr != nil {
		return runErr.Error()
	}
	return "rebuild failed"
}

// rollbackOutcome is the result of correlating a failed rebuild with the
// app's rollback history.
type rollbackOutcome struct {
	detected        bool
	restoredVersion int
}

// detectAutomaticRollback reports whether a NEW automatic rollback entry was
// recorded for the app since baseline. Valid because the rebuild held the
// deploy lock: nothing else could have rolled the app back meanwhile.
func detectAutomaticRollback(appName string, baseline int) rollbackOutcome {
	records, _, err := deploy.ReadRollbackHistory(appName)
	if err != nil {
		return rollbackOutcome{}
	}
	for i := len(records) - 1; i >= baseline && i >= 0; i-- {
		rec := records[i]
		if rec.Source == deploy.RollbackSourceAutomatic {
			out := rollbackOutcome{detected: true}
			out.restoredVersion = parseVersionLabel(rec.To)
			return out
		}
	}
	return rollbackOutcome{}
}

func rollbackHistoryLen(appName string) int {
	records, _, err := deploy.ReadRollbackHistory(appName)
	if err != nil {
		return 0
	}
	return len(records)
}

// correlateDeployedVersion finds the Phelix version the rebuild produced for
// this job: the app's current version when it was promoted from exactly the
// pushed commit. This ties delivery → job → commit → version → outcome
// without ever re-deriving the commit from the branch.
func correlateDeployedVersion(job *Job) int {
	meta, err := deploy.CurrentVersionMeta(job.AppName)
	if err != nil || meta == nil {
		return 0
	}
	if !strings.EqualFold(meta.GitCommit, job.CommitSHA) {
		return 0
	}
	return meta.Version
}

// parseVersionLabel converts a rollback history label ("v14") to its number.
func parseVersionLabel(label string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(label), "v"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// waitForDeployLock blocks until no other operation holds the app's deploy
// lock (manual rebuild, rollback, remote command) or ctx is done. It only
// probes the lock — it never takes it, so the webhook can never exclude the
// rebuild it is about to spawn.
func (c *CliRebuild) waitForDeployLock(ctx context.Context, job *Job) error {
	for {
		holder, err := deploy.TryLoadLock(job.AppName)
		if err != nil {
			return phelixerr.Wrapf(phelixerr.CodeFilesystem, err,
				"webhook: could not inspect deploy lock for app %s", job.AppName)
		}
		if holder == nil {
			return nil
		}
		logs.Debug("webhook", "app=%s job=%s waiting for deploy lock held by %q (pid %d, since %s)",
			job.AppName, jobRecordID(job), holder.Operation, holder.PID, holder.StartedAt.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			return phelixerr.Wrap(phelixerr.CodeUnavailable,
				"webhook: server shutdown while waiting for the deploy lock", ErrShutdown)
		case <-time.After(c.poll):
		}
	}
}

// lockContention reports whether a failed rebuild attempt lost the deploy
// lock race: either the rebuild's own error says so (the marker the rebuild
// command wraps lock failures in), or another operation holds the lock right
// now.
func (c *CliRebuild) lockContention(job *Job, stderrTail []string) bool {
	for _, line := range stderrTail {
		if strings.Contains(line, deployLockFailureMarker) {
			return true
		}
	}
	holder, err := deploy.TryLoadLock(job.AppName)
	return err == nil && holder != nil
}

// lineTail keeps the last n lines written to it.
type lineTail struct {
	mu    sync.Mutex
	lines []string
	n     int
}

func newLineTail(n int) *lineTail { return &lineTail{n: n} }

func (t *lineTail) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	if s == "" {
		return len(p), nil
	}
	t.mu.Lock()
	t.lines = append(t.lines, s)
	if len(t.lines) > t.n {
		t.lines = t.lines[len(t.lines)-t.n:]
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *lineTail) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.lines))
	copy(out, t.lines)
	return out
}

// lineLogger writes each received line to the self log, so webhook-triggered
// rebuild output stays diagnosable through phelix.log exactly like the
// monitor daemon's subprocess output.
type lineLogger struct {
	component string
	prefix    string
}

func newLineLogger(component, prefix string) *lineLogger {
	return &lineLogger{component: component, prefix: prefix}
}

func (l *lineLogger) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	if s != "" {
		logs.Debug(l.component, "%s %s", l.prefix, s)
	}
	return len(p), nil
}
