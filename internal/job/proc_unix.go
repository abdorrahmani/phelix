//go:build !windows

package job

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the attempt in its own process group so a timeout can
// terminate the command and every child it spawned.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup terminates the whole group. It falls back to the direct
// process when the group is already gone.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
