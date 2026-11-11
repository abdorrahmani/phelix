package job

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/deploy"
	envpkg "github.com/abdorrahmani/phelix/internal/env"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/secrets"
)

// Concurrency policies. The scheduler and CLI pass the resolved value.
const (
	ConcurrencyForbid = "forbid"
	ConcurrencyAllow  = "allow"
)

// DefaultTimeout bounds one attempt when the caller leaves it unset.
const DefaultTimeout = 30 * time.Minute

// RunRequest is one execution request.
type RunRequest struct {
	AppName      string
	AppID        string
	Dir          string
	Command      []string
	Timeout      time.Duration
	Retries      int
	Concurrency  string
	Trigger      Trigger
	ScheduledFor *time.Time
	Version      int
}

// Executor runs job commands and records them.
type Executor struct {
	Store *Store
	// Now makes the clock injectable for tests.
	Now func() time.Time
	// Log receives diagnostics, never command output.
	Log func(level string, format string, args ...any)
}

// NewExecutor builds an executor over the given store.
func NewExecutor(store *Store) *Executor {
	return &Executor{Store: store, Now: time.Now}
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) log(level, format string, args ...any) {
	if e.Log != nil {
		e.Log(level, format, args...)
	}
}

// Run executes one request. Command failures are recorded, not returned as
// errors; setup failures and a held run lock are returned. The returned record
// carries the final status whenever one could be persisted.
func (e *Executor) Run(ctx context.Context, req RunRequest) (*Record, error) {
	if err := deploy.ValidateAppName(req.AppName); err != nil {
		return nil, err
	}
	if req.Dir == "" {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "job: project directory is required")
	}
	if len(req.Command) == 0 {
		return nil, phelixerr.New(phelixerr.CodeInvalidArgument, "job: command is required")
	}
	if req.Timeout <= 0 {
		req.Timeout = DefaultTimeout
	}
	if req.Retries < 0 {
		req.Retries = 0
	}
	if req.Trigger == "" {
		req.Trigger = TriggerScheduled
	}
	if req.Version == 0 {
		if current, err := deploy.CurrentVersion(req.AppName); err == nil {
			req.Version = current
		}
	}

	var lock *RunLock
	if req.Concurrency != ConcurrencyAllow {
		var err error
		lock, err = e.Store.TryAcquireRunLock(req.AppName)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
	}

	exe, args, err := resolveCommand(req.Dir, req.Command, e.versionBinary(req.AppName, req.Version))
	if err != nil {
		return nil, err
	}
	env, release, err := e.environment(req)
	if err != nil {
		return nil, err
	}
	defer release()

	id := NewID()
	logFile, err := e.Store.OpenLog(req.AppName, id)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()

	started := e.now()
	record := &Record{
		ID:        id,
		AppName:   req.AppName,
		AppID:     req.AppID,
		Trigger:   req.Trigger,
		Status:    StatusRunning,
		Command:   append([]string(nil), req.Command...),
		Version:   req.Version,
		StartedAt: started,
	}
	if req.ScheduledFor != nil {
		scheduled := *req.ScheduledFor
		record.ScheduledFor = &scheduled
	}
	if err := e.Store.Save(record); err != nil {
		return nil, err
	}

	status := StatusFailed
	total := req.Retries + 1
	for attempt := 1; attempt <= total; attempt++ {
		if ctx.Err() != nil {
			status = StatusCancelled
			record.Error = "cancelled"
			break
		}
		attemptStatus, exitCode, message := e.attempt(ctx, req, exe, args, env, logFile, record)
		record.Attempts = attempt
		record.ExitCode = exitCode
		record.Error = message
		status = attemptStatus
		if attemptStatus == StatusSucceeded || attemptStatus == StatusCancelled {
			break
		}
	}
	if status == StatusSucceeded {
		record.Error = ""
	}

	finished := e.now()
	record.Status = status
	record.FinishedAt = &finished
	record.DurationMS = finished.Sub(started).Milliseconds()
	if err := e.Store.Save(record); err != nil {
		return record, err
	}
	if err := e.Store.Retain(req.AppName, Retention); err != nil {
		e.log("warning", "job: %s: could not apply retention: %v", req.AppName, err)
	}
	return record, nil
}

// Skip records a scheduled occurrence that was deliberately not started.
func (e *Executor) Skip(req RunRequest, scheduledFor time.Time, reason string) (*Record, error) {
	if err := deploy.ValidateAppName(req.AppName); err != nil {
		return nil, err
	}
	now := e.now()
	scheduled := scheduledFor
	record := &Record{
		ID:           NewID(),
		AppName:      req.AppName,
		AppID:        req.AppID,
		Trigger:      TriggerScheduled,
		Status:       StatusSkipped,
		Command:      append([]string(nil), req.Command...),
		Version:      req.Version,
		StartedAt:    now,
		FinishedAt:   &now,
		ScheduledFor: &scheduled,
		Error:        reason,
	}
	if err := e.Store.Save(record); err != nil {
		return nil, err
	}
	return record, nil
}

// attempt runs one try and appends its output to the execution log.
func (e *Executor) attempt(ctx context.Context, req RunRequest, exe string, args, env []string, logFile *os.File, record *Record) (Status, int, string) {
	shared := &lockedWriter{w: logFile}
	stdout := logs.NewAppLogWriter(shared, logs.StreamStdout)
	stderr := logs.NewAppLogWriter(shared, logs.StreamStderr)

	cmd := exec.Command(exe, args...)
	cmd.Dir = req.Dir
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return StatusFailed, -1, phelixerr.Redact(err.Error())
	}
	record.PID = cmd.Process.Pid
	if identity, err := processex.Identity(cmd.Process.Pid); err == nil {
		record.ProcessStart = identity.StartTime
		record.Executable = identity.ExePath
	} else {
		record.Executable = exe
	}
	if err := e.Store.Save(record); err != nil {
		e.log("warning", "job: %s: could not record the running process: %v", req.AppName, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(req.Timeout)
	defer timer.Stop()

	status := StatusFailed
	message := ""
	select {
	case waitErr := <-done:
		if waitErr == nil {
			status = StatusSucceeded
		} else {
			message = phelixerr.Redact(waitErr.Error())
		}
	case <-timer.C:
		killProcessGroup(cmd)
		<-done
		status = StatusTimedOut
		message = "attempt timed out after " + req.Timeout.String()
	case <-ctx.Done():
		killProcessGroup(cmd)
		<-done
		status = StatusCancelled
		message = "cancelled"
	}

	_ = stdout.Close()
	_ = stderr.Close()

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	return status, exitCode, message
}

// versionBinary returns the current built binary when the requested version
// is unset or unavailable.
func (e *Executor) versionBinary(appName string, version int) string {
	ver := version
	if ver == 0 {
		current, err := deploy.CurrentVersion(appName)
		if err != nil {
			return ""
		}
		ver = current
	}
	if ver <= 0 {
		return ""
	}
	binary, _, err := deploy.VersionPaths(appName, ver)
	if err != nil {
		return ""
	}
	return binary
}

// environment builds the child environment: the parent env, the version's env
// snapshot (or the live env store before the first build), and the app's
// committed secret bindings.
func (e *Executor) environment(req RunRequest) ([]string, func(), error) {
	envPath := ""
	if req.Version > 0 {
		if _, path, err := deploy.VersionPaths(req.AppName, req.Version); err == nil {
			envPath = path
		}
	} else if current, err := deploy.CurrentVersion(req.AppName); err == nil && current > 0 {
		if _, path, err := deploy.VersionPaths(req.AppName, current); err == nil {
			envPath = path
		}
	}
	if envPath == "" && req.AppID != "" {
		if path, err := envpkg.GetEnvFilePath(req.AppID); err == nil {
			envPath = path
		}
	}
	overlay, release, err := deploy.EnvOverlayFromSnapshot(envPath, req.AppID)
	if err != nil {
		return nil, func() {}, err
	}
	return secrets.MergeEnv(os.Environ(), overlay), release, nil
}

// resolveCommand picks the executable: an absolute or relative path under the
// project directory, a bare name on PATH, or the current built binary when a
// relative command such as ./backup no longer exists after a build.
func resolveCommand(dir string, command []string, versionBinary string) (string, []string, error) {
	exe := command[0]
	args := command[1:]
	if strings.TrimSpace(exe) == "" {
		return "", nil, phelixerr.New(phelixerr.CodeInvalidArgument, "job: command executable is empty")
	}

	pathLike := filepath.IsAbs(exe) || strings.ContainsAny(exe, `/\`) || strings.HasPrefix(exe, ".")
	if pathLike {
		candidate := exe
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(dir, candidate)
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, args, nil
		}
		if versionBinary != "" {
			return versionBinary, args, nil
		}
		return "", nil, phelixerr.Newf(phelixerr.CodeNotFound,
			"job: command %q not found in %s and no built version is available", exe, dir)
	}

	candidate := filepath.Join(dir, exe)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate, args, nil
	}
	if _, err := exec.LookPath(exe); err != nil {
		return "", nil, phelixerr.Newf(phelixerr.CodeNotFound,
			"job: command %q not found on PATH or in %s", exe, dir)
	}
	return exe, args, nil
}

// lockedWriter serializes complete log lines from stdout and stderr.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
