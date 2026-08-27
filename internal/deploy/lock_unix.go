//go:build !windows

package deploy

import (
	"os"
	"syscall"
)

// lockFileExclusive takes an exclusive non-blocking BSD flock on f.
// It fails with syscall.EWOULDBLOCK when another holder owns the lock.
// The kernel releases the lock automatically if this process dies, so no
// stale-lock cleanup pass is ever needed.
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlockFile releases the exclusive lock taken by lockFileExclusive.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
