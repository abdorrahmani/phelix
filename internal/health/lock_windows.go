//go:build windows

package health

import (
	"os"
	"syscall"
)

const lockfileExclusiveLock = 2

// lockRangeBytes is the byte range locked inside health.lock. The file only
// ever holds a small marker, so 4 KiB is plenty.
const lockRangeBytes = 4096

// lockFileExclusive takes a BLOCKING exclusive byte-range lock over the whole
// file via LockFileEx (no fail-immediately flag — health writers queue rather
// than fail; see the unix sibling). The OS releases it if the process dies.
//
// Sibling of internal/deploy/lock_windows.go; duplicated because
// internal/deploy imports internal/health (a shared helper would cycle).
func lockFileExclusive(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.LockFileEx(f.Fd(), lockfileExclusiveLock, 0, lockRangeBytes, 0, &ol)
}

// unlockFile releases the exclusive lock taken by lockFileExclusive.
func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.UnlockFileEx(f.Fd(), 0, lockRangeBytes, 0, &ol)
}
