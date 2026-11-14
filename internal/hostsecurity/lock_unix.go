//go:build !windows

package hostsecurity

import (
	"os"
	"syscall"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// acquireHostLock takes a BSD flock on path, blocking when requested. Lock
// files are companions of the scan store: they are created once and never
// renamed, so every process contends on the same inode. The kernel releases
// the lock if the process dies, so a stale lock is impossible.
func acquireHostLock(path string, blocking bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "host security: open lock %s", path)
	}
	if blocking {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "host security: lock %s", path)
		}
		return f, nil
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrHostScanLockHeld
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "host security: lock %s", path)
	}
	return f, nil
}

// releaseHostLock releases the lock taken by acquireHostLock.
func releaseHostLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// probeHostLock reports whether another process holds an exclusive flock on
// path. It opens the file read-only and never creates it: probing a missing
// manager lock file must leave the filesystem untouched. flock works on
// read-only descriptors. The probe is best-effort on both sides: a missing
// or unreadable file proves nothing, and dpkg, apt, and rpm guard their
// databases with fcntl locks rather than flock, so a quiet probe does not
// prove the database is free (the post-hoc manager output matching is the
// reliable signal).
func probeHostLock(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK {
			return true, nil
		}
		return false, err
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}
