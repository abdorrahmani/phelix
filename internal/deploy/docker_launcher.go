package deploy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// dockerInternalPort is the port the managed app is told to bind (via PORT)
// INSIDE its container. Each container has its own network namespace, so a
// fixed internal port never collides across instances; Docker maps it to an
// ephemeral host port that the proxy dials on 127.0.0.1. This keeps the PORT
// contract identical to the native launcher: the app reads PORT and binds it,
// unaware that a container boundary exists.
const dockerInternalPort = 8080

// DockerLabelApp / DockerLabelSlot tag every container Phelix launches so the
// runtime can find, verify, and reclaim its own containers (phase 4 recovery)
// without ever touching a container it did not create.
const (
	DockerLabelApp     = "phelix.app"
	DockerLabelSlot    = "phelix.slot"
	DockerLabelManaged = "phelix.managed"
)

// dockerRunner is the command seam. Production uses execDockerRunner; tests
// inject a fake so no test needs a real Docker daemon. It mirrors the
// CommandContext pattern in internal/matrix/dockerbuild.go rather than
// syscmd.Runner (whose allowlist is fixed to systemctl/sudo).
type dockerRunner func(ctx context.Context, args ...string) (stdout string, err error)

// execDockerRunner runs the real `docker` binary with an argv array (never a
// shell), capturing stdout. On failure it keeps the *exec.ExitError reachable
// (so errors.As still finds it) and appends a bounded, redacted stderr tail.
func execDockerRunner(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	// `docker logs` demultiplexes the container's streams: an app's panic / bind
	// error goes to the container's stderr, which docker replays on ITS stderr
	// and exits 0. Merge both into stdout for the logs subcommand so a captured
	// tail actually contains the reason a candidate died; every other subcommand
	// keeps clean, separate streams (inspect/wait output must parse cleanly).
	if len(args) > 0 && args[0] == "logs" {
		cmd.Stderr = &stdout
	} else {
		cmd.Stderr = &stderr
	}
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return stdout.String(), phelixerr.Wrapf(dockerErrCode(detail), err, "docker %s: %s",
				redactDockerArgs(args), phelixerr.Redact(detail))
		}
		return stdout.String(), phelixerr.Wrapf(phelixerr.CodeDocker, err, "docker %s", redactDockerArgs(args))
	}
	return stdout.String(), nil
}

// dockerErrCode distinguishes "the daemon isn't reachable" from a generic
// docker error, so the CLI boundary can render the actionable
// DOCKER_DAEMON_UNAVAILABLE hint (start Docker / check the socket) instead of a
// flat failure.
func dockerErrCode(stderr string) phelixerr.Code {
	s := strings.ToLower(stderr)
	if strings.Contains(s, "cannot connect to the docker daemon") ||
		strings.Contains(s, "is the docker daemon running") ||
		strings.Contains(s, "permission denied") && strings.Contains(s, "docker.sock") {
		return phelixerr.CodeDockerDaemonUnavailable
	}
	return phelixerr.CodeDocker
}

// redactDockerArgs renders a docker argv for an error/log line, masking the
// VALUES of KEY=value payloads (--label, --env). Keys stay visible so the line
// is still actionable. Env is passed via --env-file (not on argv), but this is
// defense in depth in case a caller ever inlines one.
func redactDockerArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if (a == "--label" || a == "-e" || a == "--env") && i+1 < len(args) {
			out[i] = a
			if k, _, ok := strings.Cut(args[i+1], "="); ok && k != "" {
				out[i+1] = k + "=***"
			} else {
				out[i+1] = args[i+1]
			}
			continue
		}
		out[i] = a
	}
	// The next index may have been pre-filled by the lookahead above; join as-is.
	return strings.Join(out, " ")
}

// DockerLauncherForApp returns an InstanceLauncher that runs the app's image as
// a detached container and reports the ephemeral host port Docker published, so
// the rest of the deploy flow (health probe, proxy switch, graceful stop) is
// byte-for-byte identical to the native path — it only ever sees a Process and
// a 127.0.0.1 port. In docker mode the launcher's binaryPath argument carries
// the image reference (e.g. "myapp:v12"), not a filesystem path.
func DockerLauncherForApp(appName string) InstanceLauncher {
	return newDockerLauncher(appName, "", execDockerRunner)
}

// newDockerLauncher is the test-injectable constructor. slot may be "" when the
// caller does not know it (the InstanceLauncher signature does not carry it);
// the app label alone is enough to reclaim containers, and the slot label is
// enriched opportunistically.
func newDockerLauncher(appName, slot string, run dockerRunner) InstanceLauncher {
	if run == nil {
		run = execDockerRunner
	}
	return func(ctx context.Context, imageRef string, env []string) (Process, int, error) {
		if imageRef == "" {
			return nil, 0, phelixerr.New(phelixerr.CodeInvalidArgument, "deploy: docker image reference is empty")
		}

		args := []string{"run", "-d",
			"--label", DockerLabelManaged + "=true",
			"--label", DockerLabelApp + "=" + appName,
		}
		if slot != "" {
			args = append(args, "--label", DockerLabelSlot+"="+slot)
		}
		// Publish the fixed internal port to an ephemeral host port bound to
		// loopback only — the proxy is the sole public entry point, so the
		// container must never be reachable from outside the host directly.
		args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d/tcp", dockerInternalPort))

		// Secrets are injected via a 0600 --env-file, never -e on the argv,
		// so decrypted env never appears in `ps` or the process table. PORT is
		// written LAST so an app whose env store contains PORT is still bound to
		// the internal port this launcher controls (mirrors the native launcher).
		envFile, cleanup, err := writeDockerEnvFile(env, dockerInternalPort)
		if err != nil {
			return nil, 0, err
		}
		defer cleanup()
		args = append(args, "--env-file", envFile, imageRef)

		out, err := run(ctx, args...)
		if err != nil {
			return nil, 0, phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "deploy: docker run %s", imageRef)
		}
		containerID := strings.TrimSpace(out)
		if containerID == "" {
			return nil, 0, phelixerr.New(phelixerr.CodeInstanceStartFailed, "deploy: docker run returned no container id")
		}

		proc := &dockerProcess{id: containerID, run: run}

		hostPort, err := dockerPublishedPort(ctx, run, containerID, dockerInternalPort)
		if err != nil {
			// The container started but we cannot route to it — kill it so a
			// half-started candidate is never left leaking (fail-safe deploys).
			_, _ = run(context.Background(), "kill", containerID)
			return nil, 0, err
		}

		return proc, hostPort, nil
	}
}

// writeDockerEnvFile writes KEY=VALUE lines to a temp file (0600) for
// `docker run --env-file`. The caller must invoke cleanup once docker has read
// it (docker reads the file at create time, so removing it right after
// `docker run` returns is safe). PORT is appended last.
func writeDockerEnvFile(env []string, internalPort int) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "phelix-env-*.env")
	if err != nil {
		return "", func() {}, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: create docker env file")
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: secure docker env file")
	}
	var b strings.Builder
	for _, kv := range env {
		// --env-file wants raw KEY=VALUE lines; skip anything malformed rather
		// than emit a line docker will reject.
		if strings.Contains(kv, "=") {
			b.WriteString(kv)
			b.WriteByte('\n')
		}
	}
	b.WriteString("PORT=")
	b.WriteString(strconv.Itoa(internalPort))
	b.WriteByte('\n')
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: write docker env file")
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: close docker env file")
	}
	return f.Name(), cleanup, nil
}

// dockerPublishedPort reads the ephemeral host port Docker mapped to the
// container's internal port. The proxy dials 127.0.0.1:<hostPort>.
func dockerPublishedPort(ctx context.Context, run dockerRunner, containerID string, internalPort int) (int, error) {
	format := fmt.Sprintf("{{(index (index .NetworkSettings.Ports \"%d/tcp\") 0).HostPort}}", internalPort)
	out, err := run(ctx, "inspect", "--format", format, containerID)
	if err != nil {
		return 0, phelixerr.Wrapf(phelixerr.CodeDocker, err, "deploy: inspect published port of %s", containerID)
	}
	portStr := strings.TrimSpace(out)
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return 0, phelixerr.Newf(phelixerr.CodeDocker,
			"deploy: container %s exposed no usable host port for %d/tcp (got %q)", containerID, internalPort, portStr)
	}
	return port, nil
}

// dockerProcess adapts a running container to the Process interface so
// GracefulStop drives it exactly like a native process: Signal(SIGTERM) asks
// politely, Wait blocks on `docker wait`, and Kill force-removes on timeout.
type dockerProcess struct {
	id  string
	run dockerRunner
}

// PID returns the container's main process id on the host, resolved lazily via
// docker inspect. Zero when the container is gone or its PID cannot be read.
func (p *dockerProcess) PID() int {
	out, err := p.run(context.Background(), "inspect", "--format", "{{.State.Pid}}", p.id)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return pid
}

// ContainerID exposes the underlying container id for state recording and
// label-based verification (phase 3).
func (p *dockerProcess) ContainerID() string { return p.id }

// Logs returns a bounded, redacted tail of the container's `docker logs`
// output. Deploy health-failure reporting uses it so a docker candidate that
// dies at boot surfaces its actual panic/bind error — the docker-runtime
// analogue of the native path's on-disk instance-log tail (instanceLogTail).
// Best-effort: an unreadable/empty log (or a nil runner) yields "". The runner
// merges the container's stderr into stdout for `logs` (see execDockerRunner),
// so a partial tail is still returned even when docker reports a non-zero exit.
func (p *dockerProcess) Logs(ctx context.Context, maxBytes int64) string {
	if p.id == "" || p.run == nil {
		return ""
	}
	out, _ := p.run(ctx, "logs", "--tail", "200", p.id)
	if maxBytes > 0 && int64(len(out)) > maxBytes {
		out = out[len(out)-int(maxBytes):]
	}
	tail := strings.TrimSpace(out)
	if tail == "" {
		return ""
	}
	return phelixerr.Redact(tail)
}

// containerIDOf returns the container id when proc is a docker instance, or ""
// for a native process. Instance construction uses it so a docker instance
// persists its ContainerID (and native instances stay unchanged).
func containerIDOf(proc Process) string {
	if dp, ok := proc.(interface{ ContainerID() string }); ok {
		return dp.ContainerID()
	}
	return ""
}

// dockerLogTailOf returns a redacted `docker logs` tail when proc is a docker
// instance, else "" (native processes have no Logs method). Mirrors
// containerIDOf so health.go can enrich a failure without a type switch on the
// concrete launcher.
func dockerLogTailOf(proc Process, maxBytes int64) string {
	if lp, ok := proc.(interface {
		Logs(ctx context.Context, maxBytes int64) string
	}); ok {
		return lp.Logs(context.Background(), maxBytes)
	}
	return ""
}

// Alive reports whether the container is still running, asked of Docker rather
// than the host process table. Host-PID liveness is wrong for a container: the
// PID is owned by the container's non-root user (a host signal-0 check gets
// EPERM), and under Docker-Desktop/VM or userns-remap the PID may not be on the
// host at all — both make a live container look dead and abort the deploy.
//
// A container that genuinely exited reports State.Running=false (fast-fail
// preserved); a removed container ("no such container") is likewise dead. Any
// OTHER inspect error is ambiguous (a daemon blip), so we report alive and let
// the HTTP probe and the overall health timeout stay the authority rather than
// aborting a healthy deploy on a transient error.
func (p *dockerProcess) Alive() bool {
	if p.id == "" || p.run == nil {
		return false
	}
	out, err := p.run(context.Background(), "inspect", "-f", "{{.State.Running}}", p.id)
	if err != nil {
		return !isDockerNotRunning(err)
	}
	return strings.TrimSpace(out) == "true"
}

// dockerLivenessChecker returns a health.PidChecker that consults container
// state when proc is a docker instance, or nil for a native process (so the
// caller keeps the default host-PID checker). The returned checker ignores its
// pid argument — container liveness is keyed on the container id, not the host
// PID (which is exactly the value that is unreliable for containers).
func dockerLivenessChecker(proc Process) func(pid int) bool {
	if lp, ok := proc.(interface{ Alive() bool }); ok {
		return func(int) bool { return lp.Alive() }
	}
	return nil
}

// Signal delivers sig to the container's main process without blocking, so
// GracefulStop's SIGTERM-then-wait-then-kill sequence works unchanged. The
// signal is sent by number (docker --signal accepts "15"), which avoids any
// name-mapping ambiguity across platforms.
func (p *dockerProcess) Signal(sig os.Signal) error {
	num := 0
	if s, ok := sig.(syscall.Signal); ok {
		num = int(s)
	}
	if num == 0 {
		return nil
	}
	_, err := p.run(context.Background(), "kill", "--signal="+strconv.Itoa(num), p.id)
	if err != nil && isDockerNotRunning(err) {
		// Already stopped: matches os.ErrProcessDone semantics GracefulStop tolerates.
		return os.ErrProcessDone
	}
	return err
}

// Kill force-terminates the container (SIGKILL) and removes it, so a killed
// candidate never lingers as a stopped container that a later reconcile would
// have to reap.
func (p *dockerProcess) Kill() error {
	_, err := p.run(context.Background(), "kill", p.id)
	if err != nil && isDockerNotRunning(err) {
		return os.ErrProcessDone
	}
	return err
}

// Wait blocks until the container exits and returns a non-nil error for any
// non-zero exit code, mirroring exec.Cmd.Wait so deploy's failure paths treat a
// crashed container exactly like a crashed process.
func (p *dockerProcess) Wait() error {
	out, err := p.run(context.Background(), "wait", p.id)
	if err != nil {
		// A missing container is a completed wait, not a failure to observe.
		if isDockerNotRunning(err) {
			return nil
		}
		return phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "deploy: docker wait %s", p.id)
	}
	code, convErr := strconv.Atoi(strings.TrimSpace(out))
	if convErr != nil {
		return nil
	}
	if code != 0 {
		return phelixerr.Newf(phelixerr.CodeProcessFailed, "deploy: container %s exited with code %d", p.id, code)
	}
	return nil
}

// isDockerNotRunning reports whether err indicates the container is already
// gone/stopped, which several call sites treat as "nothing left to do".
func isDockerNotRunning(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such container") ||
		strings.Contains(s, "is not running") ||
		strings.Contains(s, "is not paused")
}

// dockerStopByGrace is used when we want a bounded graceful container stop in a
// single call (docker stop --time already implements SIGTERM→wait→SIGKILL).
// GracefulStop drives Signal/Wait/Kill itself, so this is a convenience for
// call sites that only need "stop this container within N".
func dockerStopByGrace(ctx context.Context, run dockerRunner, containerID string, grace time.Duration) error {
	secs := int(grace.Seconds())
	if secs < 0 {
		secs = 0
	}
	_, err := run(ctx, "stop", "--time="+strconv.Itoa(secs), containerID)
	if err != nil && !isDockerNotRunning(err) {
		return phelixerr.Wrapf(phelixerr.CodeDocker, err, "deploy: docker stop %s", containerID)
	}
	return nil
}
