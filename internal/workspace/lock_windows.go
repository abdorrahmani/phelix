//go:build windows

package workspace

import (
	"os"
	"syscall"
)

// lockRangeBytes is the byte range locked inside workspace.lock. The file
// only ever holds a small JSON holder header, so 4 KiB is plenty.
const lockRangeBytes = 4096

// lockFileExclusive takes an exclusive, non-blocking byte-range lock over
// the whole file via LockFileEx, mirroring the deploy package. It fails when
// another holder owns the lock, and the OS releases it automatically if the
// process dies.
func lockFileExclusive(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.LockFileEx(f.Fd(), syscall.LOCKFILE_FAIL_IMMEDIATELY|syscall.LOCKFILE_EXCLUSIVE_LOCK, 0,
		lockRangeBytes, 0, &ol)
}

// unlockFile releases the exclusive lock taken by lockFileExclusive.
func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.UnlockFileEx(f.Fd(), 0, lockRangeBytes, 0, &ol)
}
