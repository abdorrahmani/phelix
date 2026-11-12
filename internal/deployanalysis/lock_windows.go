//go:build windows

package deployanalysis

import (
	"os"
	"syscall"
)

const (
	lockfileExclusiveLock = 2
	lockRangeBytes        = 1
)

func lockFileExclusive(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.LockFileEx(f.Fd(), lockfileExclusiveLock, 0, lockRangeBytes, 0, &ol)
}

func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	return syscall.UnlockFileEx(f.Fd(), 0, lockRangeBytes, 0, &ol)
}
