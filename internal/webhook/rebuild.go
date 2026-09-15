package webhook

import (
	"context"
	"io"
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

// commandRunner executes one `phelix rebuild <appID>` invocation in dir and
// returns the trailing stderr lines for failure classification. A var, not a
// func, so tests can assert which invocation a job routes to without spawning
// a process (the same pattern as internal/monitor's runPhelixCommand).
type commandRunner func(ctx context.Context, dir string, args ...string) (stderrTail []string, err error)

// runPhelixRebuildCommand is the production commandRunner: it re-invokes the
// phelix binary's rebuild command in the application's project directory —
// exactly how the monitor daemon's remote rebuild command does it — so the
// webhook job runs through the full existing pipeline (strategy selection
// from phelix.yaml, build, health verification, versioning, auto-rollback and
// the deploy lock all stay inside `phelix rebuild`).
func runPhelixRebuildCommand(ctx context.Context, dir string, args ...string) ([]string, error) {
	cmd, err := monitor.NewPhelixCommand(dir, args...)
	if err != nil {
		return nil, err
	}
	tail := newLineTail(stderrTailLines)
	cmd.Stdout = newLineLogger("webhook", "rebuild stdout:")
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
		// The rebuild subprocess is an independent CLI invocation, not a child
		// the webhook owns: leave it running so the deploy finishes cleanly
		// (it releases the deploy lock and records versions on its own). The
		// wait below only reaps the process, it does not cancel it.
		go func() { _ = cmd.Wait() }()
		return tail.snapshot(), phelixerr.Wrap(phelixerr.CodeUnavailable,
			"webhook: server shutdown; rebuild left running to completion", ErrShutdown)
	}
}

// CliRebuild executes queued webhook jobs through the existing `phelix
// rebuild` pipeline. It is the ONLY bridge between the webhook layer and the
// deployment machinery, and it deliberately contains none of that machinery:
// no build steps, no deployment strategies, no health checks, no version or
// rollback writes, and no deploy lock acquisition of its own. The per-app
// deploy lock stays authoritative inside the rebuild command; a webhook job
// whose app is locked by a manual rebuild or rollback WAITS — it never
// steals, bypasses or force-releases the lock.
type CliRebuild struct {
	poll time.Duration
	run  commandRunner
}

// NewCliRebuild returns the production rebuild service.
func NewCliRebuild() *CliRebuild {
	return &CliRebuild{poll: deployLockPollInterval, run: runPhelixRebuildCommand}
}

// Rebuild waits for the app's deploy lock to be free, then runs the rebuild
// command for the job. If the subprocess loses the tiny race between the
// lock-free observation and its own acquisition (another rebuild started in
// between), it waits and retries rather than failing.
func (c *CliRebuild) Rebuild(ctx context.Context, job *Job) error {
	for attempt := 1; ; attempt++ {
		if err := c.waitForDeployLock(ctx, job); err != nil {
			return err
		}
		stderrTail, err := c.run(ctx, job.Directory, "rebuild", job.AppID)
		if err == nil {
			return nil
		}
		if IsShutdownErr(err) {
			return err
		}
		if c.lockContention(job, stderrTail) && attempt < maxRebuildAttempts {
			logs.Info("webhook", "app=%s delivery=%s rebuild lost the deploy lock race; waiting and retrying (attempt %d/%d)",
				job.AppName, job.DeliveryID, attempt, maxRebuildAttempts)
			continue
		}
		return err
	}
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
		logs.Debug("webhook", "app=%s delivery=%s waiting for deploy lock held by %q (pid %d, since %s)",
			job.AppName, job.DeliveryID, holder.Operation, holder.PID, holder.StartedAt.Format(time.RFC3339))
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
