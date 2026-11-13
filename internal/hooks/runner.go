package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
)

const MaxOutputBytes = 64 * 1024

const KillGrace = 5 * time.Second

type Logger interface {
	Stepf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Successf(format string, args ...any)
	Errorf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Stepf(string, ...any)    {}
func (nopLogger) Infof(string, ...any)    {}
func (nopLogger) Warnf(string, ...any)    {}
func (nopLogger) Successf(string, ...any) {}
func (nopLogger) Errorf(string, ...any)   {}

type Runner struct {
	AppName         string
	AppID           string
	ProjectDir      string
	DeploymentID    string
	Strategy        string
	Version         int
	PreviousVersion int
	Logger          Logger
	Store           *Store
	BaseEnv         []string
	Now             func() time.Time
}

type Result struct {
	Execution *Execution
	Err       error
}

// Failed reports whether the hook failed. A skipped hook did not fail.
func (r Result) Failed() bool {
	return r.Err != nil || (r.Execution != nil && (r.Execution.Status == StatusFailed ||
		r.Execution.Status == StatusTimedOut || r.Execution.Status == StatusUncertain))
}

// Aborted reports whether the deployment must stop. It is false when the
// configured failure policy permits continuation.
func (r Result) Aborted() bool {
	if !r.Failed() || r.Execution == nil {
		return r.Failed()
	}
	return !r.Execution.ContinueOnError
}

func (r *Runner) logger() Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return nopLogger{}
}

func (r *Runner) store() *Store {
	if r.Store != nil {
		return r.Store
	}
	return NewStore(r.AppName)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run executes one hook with retries. It returns a Result for every outcome,
// including configuration and duplicate-protection refusals. The caller aborts
// the deployment when Result.Aborted reports true.
func (r *Runner) Run(ctx context.Context, name string, cfg *project.HookConfig) (out Result) {
	if cfg == nil || !cfg.IsEnabled() {
		return Result{Execution: &Execution{AppName: r.AppName, DeploymentID: r.DeploymentID, Hook: name, Status: StatusSkipped}}
	}
	if r.ProjectDir == "" {
		return r.refuse(name, cfg, "the project directory is unknown")
	}
	workDir, err := resolveWorkingDir(r.ProjectDir, cfg.WorkingDir)
	if err != nil {
		return r.refuse(name, cfg, err.Error())
	}

	store := r.store()
	existing, err := store.Find(r.DeploymentID, name)
	if err != nil {
		return r.refuse(name, cfg, "could not read hook history: "+err.Error())
	}
	for _, prior := range existing {
		if prior.Status == StatusSucceeded {
			r.logger().Infof("hook %s already completed for this deployment; not running it again", name)
			return Result{Execution: prior}
		}
		if prior.Status == StatusUncertain || prior.Status == StatusRunning || prior.Status == StatusPending {
			if !IsTerminal(prior.Status) {
				prior.Status = StatusUncertain
				prior.FailureReason = "execution was interrupted and its outcome cannot be established; it was not retried automatically"
				_ = store.Save(prior)
			}
			msg := fmt.Sprintf("hook %s has an uncertain prior execution (%s); refusing to run it again. Review 'phelix hooks history %s' and resolve it manually", name, prior.ID, r.AppName)
			r.logger().Errorf("%s", msg)
			return Result{Execution: prior, Err: phelixerr.New(phelixerr.CodeDeployLocked, msg)}
		}
	}

	timeout := cfg.TimeoutDuration()
	retryInterval := cfg.RetryIntervalDuration()
	attempts := 1 + cfg.Retries
	var last Result
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Result{Execution: &Execution{
				ID: newExecutionID(r.DeploymentID, name, attempt), AppName: r.AppName, AppID: r.AppID,
				DeploymentID: r.DeploymentID, Strategy: r.Strategy, Hook: name, Attempt: attempt,
				Version: r.Version, PreviousVersion: r.PreviousVersion,
				Command: cfg.Command, WorkingDir: workDir, Status: StatusCancelled,
				StartedAt: r.now(), FinishedAt: r.now(), ContinueOnError: cfg.ContinuesOnError(name),
				FailureReason: "cancelled before the attempt started",
			}}
		}
		last = r.attempt(ctx, name, cfg, workDir, attempt, timeout)
		if last.Err == nil || last.Execution == nil || last.Execution.Status == StatusCancelled {
			return last
		}
		if attempt < attempts {
			delay := retryInterval
			for i := 1; i < attempt; i++ {
				delay *= 2
			}
			if delay > timeout {
				delay = timeout
			}
			r.logger().Warnf("hook %s failed (attempt %d/%d); retrying in %s", name, attempt, attempts, delay)
			select {
			case <-ctx.Done():
				last.Execution.Status = StatusCancelled
				last.Execution.FailureReason = "cancelled during retry backoff"
				last.Execution.FinishedAt = r.now()
				_ = store.Save(last.Execution)
				return last
			case <-time.After(delay):
			}
		}
	}
	return last
}

func (r *Runner) refuse(name string, cfg *project.HookConfig, reason string) Result {
	exec := &Execution{
		ID: newExecutionID(r.DeploymentID, name, 1), AppName: r.AppName, AppID: r.AppID,
		DeploymentID: r.DeploymentID, Strategy: r.Strategy, Hook: name, Attempt: 1,
		Version: r.Version, PreviousVersion: r.PreviousVersion, Command: cfg.Command,
		Status: StatusFailed, StartedAt: r.now(), FinishedAt: r.now(), FailureReason: reason,
		TimeoutMS: cfg.TimeoutDuration().Milliseconds(), ContinueOnError: cfg.ContinuesOnError(name),
	}
	_ = r.store().Save(exec)
	r.logger().Errorf("hook %s refused: %s", name, reason)
	return Result{Execution: exec, Err: phelixerr.New(phelixerr.CodeConfiguration, reason)}
}

func (r *Runner) attempt(ctx context.Context, name string, cfg *project.HookConfig, workDir string, attempt int, timeout time.Duration) Result {
	store := r.store()
	record := &Execution{
		ID: newExecutionID(r.DeploymentID, name, attempt), AppName: r.AppName, AppID: r.AppID,
		DeploymentID: r.DeploymentID, Strategy: r.Strategy, Hook: name, Attempt: attempt,
		Version: r.Version, PreviousVersion: r.PreviousVersion, Command: append([]string(nil), cfg.Command...),
		WorkingDir: workDir, Status: StatusPending, StartedAt: r.now(), TimeoutMS: timeout.Milliseconds(),
		ContinueOnError: cfg.ContinuesOnError(name), OwnerPID: os.Getpid(),
	}
	_ = store.Save(record)

	env := append([]string(nil), r.baseEnv()...)
	env = append(env, Env{
		AppName: r.AppName, AppID: r.AppID, ProjectDir: r.ProjectDir,
		DeploymentID: r.DeploymentID, Strategy: r.Strategy, Hook: name,
		Version: r.Version, PreviousVersion: r.PreviousVersion, Attempt: attempt,
	}.Variables()...)
	secretValues := secretValues(cfg.Env)
	for key, value := range cfg.Env {
		env = append(env, key+"="+value)
	}

	command := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	command.Dir = workDir
	command.Env = env
	stdout := &limitedBuffer{limit: MaxOutputBytes}
	stderr := &limitedBuffer{limit: MaxOutputBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	configureProcessGroup(command)

	record.Status = StatusRunning
	record.StartedAt = r.now()
	_ = store.Save(record)
	r.logger().Stepf("running hook %s (attempt %d, timeout %s)", name, attempt, timeout)

	if err := command.Start(); err != nil {
		record.Status = StatusFailed
		record.FinishedAt = r.now()
		record.DurationMS = record.FinishedAt.Sub(record.StartedAt).Milliseconds()
		record.FailureReason = "could not start command: " + err.Error()
		_ = store.Save(record)
		r.logger().Errorf("hook %s failed to start: %v", name, err)
		return Result{Execution: record, Err: phelixerr.Wrap(phelixerr.CodeProcessFailed, "hook failed to start", err)}
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	var runErr error
	status := StatusSucceeded
	reason := ""
	select {
	case err := <-done:
		if err != nil {
			status = StatusFailed
			reason = err.Error()
			runErr = phelixerr.Wrap(phelixerr.CodeProcessFailed, "hook failed", err)
		}
	case <-ctx.Done():
		status = StatusCancelled
		reason = "cancelled by the caller"
		terminateProcessGroup(command)
		select {
		case <-done:
		case <-time.After(KillGrace):
			killProcessGroup(command)
			<-done
		}
		runErr = phelixerr.Wrap(phelixerr.CodeCancelled, "hook cancelled", ctx.Err())
	case <-timer:
		status = StatusTimedOut
		reason = fmt.Sprintf("timeout after %s", timeout)
		terminateProcessGroup(command)
		select {
		case <-done:
		case <-time.After(KillGrace):
			killProcessGroup(command)
			<-done
		}
		runErr = phelixerr.Newf(phelixerr.CodeTimeout, "hook %s timed out after %s", name, timeout)
	}

	record.FinishedAt = r.now()
	record.DurationMS = record.FinishedAt.Sub(record.StartedAt).Milliseconds()
	record.Stdout = redact(stdout.String(), secretValues, cfg.Env)
	record.Stderr = redact(stderr.String(), secretValues, cfg.Env)
	record.StdoutTruncated = stdout.truncated
	record.StderrTruncated = stderr.truncated
	record.Status = status
	record.FailureReason = reason
	if command.ProcessState != nil {
		code := command.ProcessState.ExitCode()
		record.ExitCode = &code
	}
	if status == StatusFailed && record.ExitCode != nil {
		record.FailureReason = fmt.Sprintf("exit code %d", *record.ExitCode)
	}
	_ = store.Save(record)

	switch status {
	case StatusSucceeded:
		r.logger().Successf("hook %s completed in %s", name, time.Duration(record.DurationMS)*time.Millisecond)
	case StatusCancelled:
		r.logger().Warnf("hook %s cancelled", name)
	case StatusTimedOut:
		r.logger().Errorf("hook %s timed out after %s", name, timeout)
	default:
		r.logger().Errorf("hook %s failed: %s", name, record.FailureReason)
	}
	if runErr != nil && record.ContinueOnError {
		r.logger().Warnf("hook %s failed but continue_on_error is set; the deployment continues", name)
	}
	return Result{Execution: record, Err: runErr}
}

func (r *Runner) baseEnv() []string {
	if r.BaseEnv != nil {
		return r.BaseEnv
	}
	return os.Environ()
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var _ io.Writer = (*limitedBuffer)(nil)

// secretValues collects values that must never appear verbatim in captured
// output. Names that look like credentials are treated as secrets.
func secretValues(env map[string]string) []string {
	var out []string
	for key, value := range env {
		if value == "" || !looksSecret(key) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func looksSecret(key string) bool {
	k := strings.ToLower(key)
	for _, marker := range []string{"secret", "token", "password", "passwd", "apikey", "api_key", "credential", "private_key", "access_key", "database_url", "dsn"} {
		if strings.Contains(k, marker) {
			return true
		}
	}
	return false
}

func redact(text string, values []string, env map[string]string) string {
	for _, value := range values {
		if value == "" {
			continue
		}
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}
	for key, value := range env {
		if value == "" || !looksSecret(key) {
			continue
		}
		text = strings.ReplaceAll(text, value, "[REDACTED]")
	}
	return text
}

var ErrHistoryNotFound = errors.New("hook execution not found")
