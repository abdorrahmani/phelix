//go:build !unix

package deploy

import (
	"os/exec"
)

// startProbeCommand launches the probe on platforms without process groups.
// shortcut: no group kill, a spawning probe may outlive its timeout on
// non-unix platforms; add a platform equivalent when workers are supported there.
func startProbeCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

func killProbe(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func probeFailureDetail(err error, output string) string {
	if output != "" {
		return output
	}
	return "exit: " + err.Error()
}
