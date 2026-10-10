//go:build windows

package session

import (
	"os"
	"syscall"
)

const (
	// lockfileExclusiveLock requests an exclusive lock. The absence of
	// LOCKFILE_FAIL_IMMEDIATELY (value 1) makes LockFileEx BLOCK until the lock
	// is acquired, matching the Unix blocking flock: session updates serialize
	// rather than fail so no update is lost.
	lockfileExclusiveLock = 2
	// lockRangeBytes is the byte range locked inside <id>.lock. The file holds
	// no payload, so a small fixed range is plenty.
	lockRangeBytes = 4096
)

// lockFileBlocking takes an exclusive, blocking byte-range lock over the lock
// file via LockFileEx. The OS releases it automatically if this process dies.
func lockFileBlocking(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.LockFileEx(syscall.Handle(f.Fd()), lockfileExclusiveLock, 0, lockRangeBytes, 0, &ol)
}

// unlockFile releases the exclusive lock taken by lockFileBlocking.
func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.UnlockFileEx(syscall.Handle(f.Fd()), 0, lockRangeBytes, 0, &ol)
}
