//go:build unix

package deploy

import (
	"os/exec"
	"syscall"
)

// startProbeCommand launches the probe in its own process group so a timeout
// can kill the whole tree (a probe that spawns children must never leak them).
func startProbeCommand(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Start()
}

// killProbe force-kills the probe's whole process group.
func killProbe(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// probeFailureDetail renders one failed probe for status and logs.
func probeFailureDetail(err error, output string) string {
	if output != "" {
		return output
	}
	return "exit: " + err.Error()
}
