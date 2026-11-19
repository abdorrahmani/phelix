//go:build !windows

package logs

import (
	"os"
	"syscall"
)

func acquireDockerDrain(file *os.File) bool {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
