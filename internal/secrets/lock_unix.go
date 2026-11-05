//go:build !windows

package secrets

import (
	"os"
	"syscall"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// acquireStoreLock takes a BSD flock on the app's lock file, blocking until
// it is available. The lock file is a small, separate companion of the store:
// because the store itself is replaced by rename on every save, a lock held
// on the store file would silently split across two inodes. The lock file is
// created once and never replaced, so every process contends on the same
// inode for the whole life of the installation. The kernel releases the lock
// if this process dies, so a stale lock is impossible.
func acquireStoreLock(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: open lock %s", path)
	}
	if err := checkPrivateFile(f); err != nil {
		f.Close()
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: lock %s", path)
	}
	info, err := f.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(info, current) {
		releaseStoreLock(f)
		f.Close()
		return nil, phelixerr.New(phelixerr.CodeFilesystem, "secrets: lock file changed while waiting")
	}
	return f, nil
}

// releaseStoreLock releases the lock taken by acquireStoreLock.
func releaseStoreLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
