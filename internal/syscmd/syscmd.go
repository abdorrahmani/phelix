// Package syscmd executes the small, fixed set of system binaries the
// updater needs: systemctl (service control) and sudo (privilege
// escalation).
//
// Security contract:
//   - Every executed program is named as a compile-time literal in the
//     switch below; any other name is rejected. No shell is ever executed.
//   - Arguments are always passed as an argv array, never concatenated into
//     a command string.
//   - Callers must pass only locally-constructed paths (never content
//     derived from untrusted input) as arguments.
package syscmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes external commands. ExecRunner is the production
// implementation; tests substitute fakes so no test needs real systemd or
// sudo.
type Runner interface {
	Run(name string, args ...string) (stdout string, err error)
}

// ExecRunner is the production Runner.
type ExecRunner struct{}

func (ExecRunner) Run(name string, args ...string) (string, error) {
	var cmd *exec.Cmd
	switch name {
	case "systemctl":
		cmd = exec.Command("systemctl", args...)
	case "sudo":
		// sudo prompts on the controlling terminal and may need stdin for
		// password entry, so it keeps the caller's stdin.
		cmd = exec.Command("sudo", args...)
		cmd.Stdin = os.Stdin
	default:
		return "", fmt.Errorf("syscmd: command %q is not allowed", name)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, detail)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}
