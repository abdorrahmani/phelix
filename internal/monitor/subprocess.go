package monitor

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strings"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// NewPhelixCommand builds the `phelix <args...>` subprocess for operations
// that run through the CLI (the monitor's remote commands, the webhook
// rebuild trigger), running it in dir so the command picks up that app's
// phelix.yaml. An empty or missing dir falls back to the daemon's own working
// directory rather than failing the command.
//
// The daemon *is* a phelix process, so its own executable is the correct binary
// to re-invoke: same version, and no dependence on PATH. Under systemd PATH
// routinely excludes the install directory, which made every subprocess-backed
// remote command fail with "phelix executable not found in PATH". PATH is kept
// only as a fallback for the rare case os.Executable cannot resolve.
func NewPhelixCommand(dir string, args ...string) (*exec.Cmd, error) {
	phelixPath, err := os.Executable()
	if err != nil || phelixPath == "" {
		phelixPath, err = exec.LookPath("phelix")
		if err != nil {
			return nil, phelixerr.Wrap(phelixerr.CodeProcessFailed, "phelix executable not found in PATH", err)
		}
	}

	execCmd := exec.Command(phelixPath, args...)

	// Set up environment with Go variables. The rebuild subprocess compiles
	// apps, so it needs a working Go toolchain even under systemd, where PATH
	// is minimal. GOROOT is resolved instead of hardcoded: an explicit GOROOT
	// wins, then the toolchain that built this binary, then the historical
	// /usr/local/go default — but only when the directory actually exists, so
	// a stale hardcoded path can never shadow a working toolchain already on
	// PATH (Arch Linux e.g. installs Go under /usr/lib/go).
	env := os.Environ()
	goroot := firstExistingDir(os.Getenv("GOROOT"), runtime.GOROOT(), "/usr/local/go")
	if goroot != "" {
		env = append(env, "GOROOT="+goroot)
	}
	gopath := os.Getenv("HOME") + "/go"
	env = append(env, "GOPATH="+gopath)

	pathPrefixes := make([]string, 0, 2)
	if goroot != "" {
		pathPrefixes = append(pathPrefixes, goroot+"/bin")
	}
	pathPrefixes = append(pathPrefixes, gopath+"/bin")
	env = append(env, "PATH="+strings.Join(pathPrefixes, ":")+":"+os.Getenv("PATH"))

	execCmd.Env = env
	execCmd.Dir = "."
	if dir != "" {
		if st, serr := os.Stat(dir); serr == nil && st.IsDir() {
			execCmd.Dir = dir
		}
	}

	return execCmd, nil
}

// firstExistingDir returns the first argument that is an existing directory,
// or "" when none qualifies.
func firstExistingDir(candidates ...string) string {
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// runPhelixCommand pipes the subprocess's stdout/stderr to the daemon log and
// waits for it to finish. Any captured output is surfaced so backend-issued
// commands remain diagnosable through journalctl / phelix.log.
//
// A var, not a func, so tests can assert which `phelix` invocation a remote
// command routes to without spawning a process.
var runPhelixCommand = func(execCmd *exec.Cmd) error {
	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to create stdout pipe", err)
	}
	stderr, err := execCmd.StderrPipe()
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to create stderr pipe", err)
	}

	if err := execCmd.Start(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "failed to start command", err)
	}

	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			logs.Debug("monitor", "command stdout: %s", scanner.Text())
		}
	}()
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logs.Debug("monitor", "command stderr: %s", scanner.Text())
		}
	}()

	if err := execCmd.Wait(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeProcessFailed, "command failed", err)
	}

	<-stdoutDone
	<-stderrDone

	return nil
}
