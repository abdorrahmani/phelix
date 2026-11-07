//go:build windows

package security

import (
	"os"

	"golang.org/x/sys/windows"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// acquireLock opens the lock file on platforms without BSD flock and takes
// a mandatory range lock. Serialization between processes is provided by
// the atomic replacement of the data files themselves on these platforms.
func acquireLock(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: open lock %s", path)
	}
	flags := uint32(0)
	if exclusive {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "security: cannot take scan lock", err)
	}
	return f, nil
}

// acquireLockTry takes the exclusive range lock without blocking. A held
// lock reports ErrScanLockHeld.
func acquireLockTry(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "security: open lock %s", path)
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		return nil, ErrScanLockHeld
	}
	return f, nil
}

// releaseLock releases the lock taken by acquireLock.
func releaseLock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
