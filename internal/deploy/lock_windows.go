//go:build windows

package deploy

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 1
	lockfileExclusiveLock   = 2
)

// lockRangeBytes is the byte range we lock inside deploy.lock. The file only
// ever holds a small JSON header, so 4 KiB is plenty.
const lockRangeBytes = 4096

// lockFileExclusive takes an exclusive non-blocking byte-range lock over the
// whole file via LockFileEx. It fails when another holder owns the lock. The
// OS releases the lock automatically if this process dies.
func lockFileExclusive(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.LockFileEx(f.Fd(), lockfileFailImmediately|lockfileExclusiveLock, 0,
		lockRangeBytes, 0, &ol)
}

// unlockFile releases the exclusive lock taken by lockFileExclusive.
func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.UnlockFileEx(f.Fd(), 0, lockRangeBytes, 0, &ol)
}
