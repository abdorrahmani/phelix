//go:build !windows

package workspace

import (
	"syscall"

	"os"
)

// lockFileExclusive takes an exclusive, non-blocking OS file lock. Contended
// locks fail immediately with EWOULDBLOCK so orchestration fails fast
// instead of queueing behind a possibly-stuck operation.
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlockFile releases a lock taken by lockFileExclusive.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
