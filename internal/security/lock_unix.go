//go:build !windows

package security

import (
	"os"
	"syscall"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// acquireLock takes a BSD flock on path, blocking until it is available.
// Lock files are companions of atomically-replaced data files: they are
// created once and never renamed, so every process contends on the same
// inode. The kernel releases the lock if the process dies, so a stale lock
// is impossible.
func acquireLock(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: open lock %s", path)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: lock %s", path)
	}
	info, err := f.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(info, current) {
		f.Close()
		return nil, phelixerr.New(phelixerr.CodeFilesystem, "security: lock file changed while waiting")
	}
	return f, nil
}

// acquireLockTry takes the exclusive flock without blocking. A held lock
// reports ErrScanLockHeld so schedulers can skip instead of stall.
func acquireLockTry(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: open lock %s", path)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrScanLockHeld
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: lock %s", path)
	}
	return f, nil
}

// releaseLock releases the lock taken by acquireLock.
func releaseLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
