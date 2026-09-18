package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/resources"
	"github.com/shirou/gopsutil/process"
)

// Process is the contract deploy needs from a running instance. The real
// implementation wraps *os.Process; tests inject a fake so graceful-shutdown
// behavior is hermetic and cross-platform.
type Process interface {
	// PID returns the OS process id.
	PID() int
	// Signal sends a signal to the process (e.g. SIGTERM).
	Signal(sig os.Signal) error
	// Kill force-terminates the process (SIGKILL).
	Kill() error
	// Wait blocks until the process exits and returns its Wait status/error.
	Wait() error
}

// osProcess adapts *exec.Cmd to the Process interface.
//
// Concurrency contract: DefaultLauncher starts exactly one goroutine that runs
// cmd.Wait(); it publishes the exit status through doneCh/waitErr under mu and
// closes doneCh. Because reads of a closed channel never block and waitErr is
// immutable afterwards, any number of concurrent or sequential Wait callers
// observe the same final status safely (the previous implementation's
// receive-and-repost channel trick could deadlock concurrent waiters).
type osProcess struct {
	cmd     *exec.Cmd
	pid     int
	doneCh  chan struct{}
	mu      sync.Mutex
	waitErr error
}

func (p *osProcess) PID() int { return p.pid }

func (p *osProcess) Signal(s os.Signal) error {
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Signal(s)
	}
	return os.ErrProcessDone
}

func (p *osProcess) Kill() error {
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return os.ErrProcessDone
}

func (p *osProcess) reap(waitErr error) {
	p.mu.Lock()
	p.waitErr = waitErr
	close(p.doneCh)
	p.mu.Unlock()
}

func (p *osProcess) Wait() error {
	<-p.doneCh
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

// freePort returns a TCP port that is free at call time by asking the kernel
// for an ephemeral port (:0) and immediately closing the listener. There is an
// inherent race (another process may grab the port before we rebind it for the
// instance), but for the internal-instance use case this is acceptable and is
// the common pattern. Callers should treat an instance bind failure as a
// deploy error and retry the slot.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

// InstanceLauncher starts a backend instance and returns a Process handle plus
// the internal port it is listening on. The default implementation execs the
// built binary with PORT=<port> in its environment (apps read PORT, matching
// Phelix's existing env-injection convention). Tests inject a fake launcher.
type InstanceLauncher func(ctx context.Context, binaryPath string, env []string) (Process, int, error)

// DefaultLauncher execs the binary on a free internal port and returns it.
// Stdout/stderr are redirected to a per-instance log under ~/.phelix/logs so
// the CLI terminal is not polluted by the app's output (or by cobra help if
// the binary happens to be a CLI that exits without a subcommand).
func DefaultLauncher(ctx context.Context, binaryPath string, env []string) (Process, int, error) {
	return launchInstance(ctx, binaryPath, env, resources.Config{})
}

// LauncherForApp reads current runtime policy, never the version's artifact state.
func LauncherForApp(appName string) InstanceLauncher {
	return func(ctx context.Context, binaryPath string, env []string) (Process, int, error) {
		cfg, err := app.LoadResources(appName)
		if err != nil {
			return nil, 0, phelixerr.Wrap(phelixerr.CodeConfiguration, "deploy: load resource limits", err)
		}
		return launchInstance(ctx, binaryPath, env, cfg)
	}
}

func launchInstance(_ context.Context, binaryPath string, env []string, cfg resources.Config) (Process, int, error) {
	if binaryPath == "" {
		return nil, 0, phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: binary path is empty")
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return nil, 0, phelixerr.Wrapf(phelixerr.CodeNotFound, err, "deploy: binary not found")
	}

	port, err := freePort()
	if err != nil {
		return nil, 0, phelixerr.Wrapf(phelixerr.CodePortUnavailable, err, "deploy: allocate internal port")
	}

	logFile, err := openInstanceLog(binaryPath, port)
	if err != nil {
		return nil, 0, err
	}

	cmd := exec.Command(binaryPath)
	// Apps are expected to bind the port given via PORT. We start from the
	// parent environment (matching the existing app/process.go behaviour) and
	// overlay the caller-provided env, then set PORT LAST: an app whose env
	// store happens to contain PORT must still be started on the internal
	// port this launcher allocated, or the candidate would fight the active
	// instance for the public port and die with AddrInUse.
	cmd.Env = append(os.Environ(), env...)
	cmd.Env = append(cmd.Env, "PORT="+fmt.Sprintf("%d", port))
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	inst, err := resources.Start(cmd, cfg)
	if err != nil {
		_ = logFile.Close()
		return nil, 0, phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "deploy: start instance")
	}
	pid := cmd.Process.Pid
	memoryLimit := cfg.Memory

	errCh := make(chan struct{})
	proc := &osProcess{cmd: cmd, pid: pid, doneCh: errCh}
	go func() {
		waitErr := cmd.Wait()
		// classify a cgroup memory OOM kill before releasing the
		// cleanup lease (the watcher keeps the cgroup observable until here),
		// so Wait callers receive the resource reason instead of an
		// unexplained SIGKILL. Non-OOM exits keep the wait error unchanged.
		if inst != nil {
			oom, oomErr := inst.ResourceOOM()
			_ = inst.Close()
			if oomErr == nil && oom {
				waitErr = oomExitError(waitErr, pid, memoryLimit)
			}
		}
		_ = logFile.Close()
		proc.reap(waitErr)
	}()

	return proc, port, nil
}

// oomExitError tags an instance exit as a resource OOM kill: the instance
// cgroup's memory.events oom_kill increased during its lifetime. The original
// wait error (exit status / signal) stays in the chain.
func oomExitError(waitErr error, pid int, memoryLimit string) error {
	limit := ""
	if memoryLimit != "" {
		limit = " of " + memoryLimit
	}
	msg := fmt.Sprintf("instance (pid %d) exceeded its configured memory limit%s and was killed by the kernel OOM killer", pid, limit)
	if waitErr == nil {
		return phelixerr.New(phelixerr.CodeResourceOOM, msg)
	}
	return phelixerr.Wrapf(phelixerr.CodeResourceOOM, waitErr, "%s", msg)
}

// openInstanceLog creates (or appends to) a log file for a deploy instance.
// Falls back to os.DevNull if the logs directory cannot be created.
func openInstanceLog(binaryPath string, port int) (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.OpenFile(os.DevNull, os.O_RDWR, 0)
	}
	dir := filepath.Join(home, ".phelix", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return os.OpenFile(os.DevNull, os.O_RDWR, 0)
	}
	return os.OpenFile(instanceLogPath(home, binaryPath, port), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// instanceLogPath is the on-disk location of one deploy instance's stdout/stderr.
func instanceLogPath(home, binaryPath string, port int) string {
	name := fmt.Sprintf("deploy_%s_%d.log", filepath.Base(binaryPath), port)
	return filepath.Join(home, ".phelix", "logs", name)
}

// instanceLogTail returns the last maxBytes bytes of the instance's captured
// output, redacted. Deploy health failures include it so a candidate that
// dies at boot (bind conflict, panic, missing env) reports the actual panic
// line instead of an opaque timeout. Best-effort: missing/unreadable logs
// return "".
func instanceLogTail(binaryPath string, port int, maxBytes int64) string {
	if port <= 0 {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(instanceLogPath(home, binaryPath, port))
	if err != nil {
		return ""
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		data = data[len(data)-int(maxBytes):]
	}
	tail := strings.TrimSpace(string(data))
	if tail == "" {
		return ""
	}
	return phelixerr.Redact(tail)
}

// pidAlive reports whether the given PID is currently running, using the same
// gopsutil library the rest of Phelix uses for process introspection.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	running, err := p.IsRunning()
	return err == nil && running
}

// GracefulStop sends SIGTERM to the process and waits up to grace for it to
// exit. If it is still alive after the grace period, it is force-killed with
// SIGKILL. The returned ShutdownReport records how many requests were still
// in flight at the proxy (if known) and whether a SIGKILL was required.
//
// inFlight is the count of in-flight proxy requests at the moment we begin
// shutdown; pass 0/-1 when unknown. It is purely informational.
type ShutdownReport struct {
	ForceKilled bool
	Exited      bool
	InFlight    int64
	Elapsed     time.Duration
}

func GracefulStop(ctx context.Context, proc Process, grace time.Duration, inFlight int64) (ShutdownReport, error) {
	start := time.Now()
	report := ShutdownReport{InFlight: inFlight}

	if proc == nil {
		report.Exited = true
		return report, nil
	}

	// Ask politely.
	if err := proc.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		// Not fatal: fall through to Kill below.
		_ = err
	}

	// Wait for graceful exit, bounded by grace and the caller's ctx deadline.
	deadline := time.Now().Add(grace)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Wait() }()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case <-waitCh:
		report.Exited = true
		report.Elapsed = time.Since(start)
		return report, nil
	case <-ctx.Done():
		// Caller cancelled before grace elapsed. Still attempt hard kill so we
		// don't leak the instance.
	case <-timer.C:
	}

	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		report.Elapsed = time.Since(start)
		return report, phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "deploy: SIGKILL failed")
	}
	<-waitCh // reap
	report.ForceKilled = true
	report.Exited = true
	report.Elapsed = time.Since(start)
	return report, nil
}

// hostPort formats an internal instance address for the health checker and the
// proxy.
func hostPort(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// pidProcess adapts an *os.Process found via os.FindProcess to the Process
// interface, used for stopping instances started by a prior invocation (whose
// *os.Process we no longer hold).
type pidProcess struct {
	pid  int
	proc *os.Process
}

func (p *pidProcess) PID() int { return p.pid }
func (p *pidProcess) Signal(s os.Signal) error {
	if p.proc == nil {
		return os.ErrProcessDone
	}
	return p.proc.Signal(s)
}
func (p *pidProcess) Kill() error {
	if p.proc == nil {
		return os.ErrProcessDone
	}
	return p.proc.Kill()
}
func (p *pidProcess) Wait() error {
	if p.proc == nil {
		return os.ErrProcessDone
	}
	// Reap the zombie if we can. On a found process, Wait may not be available
	// cross-platform for a non-child; fall back to a liveness poll.
	if _, err := p.proc.Wait(); err == nil {
		return nil
	}
	// Poll until the PID disappears.
	for i := 0; i < 200; i++ {
		if !pidAlive(p.pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return phelixerr.New(phelixerr.CodeProcessFailed, "deploy: timed out waiting for pid to exit")
}

// sameExecutable reports whether a running process's resolved executable path
// refers to the same binary as want. It tolerates symlink resolution
// differences and Linux's " (deleted)" marker for replaced binaries.
func sameExecutable(exe, want string) bool {
	if exe == "" || want == "" {
		return false
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	want = strings.TrimSuffix(want, " (deleted)")
	if strings.EqualFold(filepath.Clean(exe), filepath.Clean(want)) {
		return true
	}
	// Resolve symlinks on both sides (best-effort) before comparing again.
	resolve := func(p string) string {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real
		}
		return p
	}
	return strings.EqualFold(filepath.Clean(resolve(exe)), filepath.Clean(resolve(want)))
}

// processExecutable returns the resolved executable path of pid, or "" when it
// cannot be determined (process gone, permissions, ...).
func processExecutable(pid int) string {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return ""
	}
	exe, err := p.Exe()
	if err != nil {
		return ""
	}
	return exe
}

// findVerifiedProcess locates a running process by PID and verifies its
// identity against the expected binary path before handing out a handle that
// can be signalled. This closes the classic stale-PID-recycling hazard: after
// a machine restart or heavy PID churn, deploy.json may still name a PID that
// now belongs to an unrelated process; without this check Phelix could
// SIGTERM/SIGKILL an innocent victim.
//
// Behaviour:
//   - pid <= 0 or dead pid  -> nil Process ("already stopped").
//   - expectBinary given    -> process must resolve to that exact binary;
//     otherwise nil Process ("stale record, refusing to touch").
//   - expectBinary empty    -> legacy records carry no path, so only liveness
//     is verifiable; the handle is returned unchecked.
func findVerifiedProcess(pid int, expectBinary string) Process {
	if pid <= 0 || !pidAlive(pid) {
		return nil
	}
	if expectBinary != "" && !sameExecutable(processExecutable(pid), expectBinary) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return &pidProcess{pid: pid, proc: proc}
}

// stopByPID gracefully stops an instance started by a previous CLI invocation,
// verifying first that PID still names the expected binary (see
// findVerifiedProcess). Returns Exited=true when nothing needed stopping —
// including when the record was stale so an unrelated process was protected.
func stopByPID(ctx context.Context, pid int, grace time.Duration, inFlight int64, expectBinary string) ShutdownReport {
	proc := findVerifiedProcess(pid, expectBinary)
	if proc == nil {
		return ShutdownReport{Exited: true, InFlight: inFlight}
	}
	report, _ := GracefulStop(ctx, proc, grace, inFlight)
	return report
}
