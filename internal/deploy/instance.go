package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
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

// osProcess adapts *os.Process (or *exec.Cmd) to the Process interface.
type osProcess struct {
	cmd *exec.Cmd
	pid int
	// errCh receives the result of cmd.Wait() exactly once.
	errCh chan error
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
func (p *osProcess) Wait() error {
	// First caller wins; subsequent callers get the cached result.
	if p.errCh != nil {
		err := <-p.errCh
		p.errCh <- err // re-cache for any later caller
		return err
	}
	return os.ErrProcessDone
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
func DefaultLauncher(_ context.Context, binaryPath string, env []string) (Process, int, error) {
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
	// overlay the caller-provided env plus our PORT.
	cmd.Env = append(os.Environ(), "PORT="+fmt.Sprintf("%d", port))
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, 0, phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "deploy: start instance")
	}

	errCh := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		_ = logFile.Close()
		errCh <- waitErr
	}()

	proc := &osProcess{cmd: cmd, pid: cmd.Process.Pid, errCh: errCh}
	return proc, port, nil
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
	name := fmt.Sprintf("deploy_%s_%d.log", filepath.Base(binaryPath), port)
	return os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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

// findProcess locates a running process by PID. Returns a nil Process (not an
// error) when the PID is gone, so callers can treat "already stopped" as a
// successful no-op.
func findProcess(pid int) Process {
	if pid <= 0 {
		return nil
	}
	if !pidAlive(pid) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return &pidProcess{pid: pid, proc: proc}
}

// stopByPID is the counterpart to GracefulStop for instances whose Process
// handle we don't have (started by a previous CLI invocation). It finds the
// process by PID and gracefully stops it.
func stopByPID(ctx context.Context, pid int, grace time.Duration, inFlight int64) ShutdownReport {
	proc := findProcess(pid)
	if proc == nil {
		return ShutdownReport{Exited: true, InFlight: inFlight}
	}
	report, _ := GracefulStop(ctx, proc, grace, inFlight)
	return report
}
