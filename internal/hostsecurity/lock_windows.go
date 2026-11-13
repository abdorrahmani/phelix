//go:build windows

package hostsecurity

import (
	"os"

	"golang.org/x/sys/windows"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// acquireHostLock takes a mandatory range lock on platforms without BSD
// flock, blocking when requested. Serialization between processes is
// provided by the atomic replacement of the data files themselves on these
// platforms.
func acquireHostLock(path string, blocking bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "host security: open lock %s", path)
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !blocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		if !blocking {
			return nil, ErrHostScanLockHeld
		}
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "host security: cannot take scan lock", err)
	}
	return f, nil
}

// releaseHostLock releases the lock taken by acquireHostLock.
func releaseHostLock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
