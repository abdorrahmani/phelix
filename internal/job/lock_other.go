//go:build windows

package job

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// RunLock is the exclusive cross-process lock of one job.
type RunLock struct {
	file *os.File
}

// TryAcquireRunLock takes the job's run lock without blocking. A held lock
// reports ErrRunLockHeld so the scheduler can skip instead of stall.
func (s *Store) TryAcquireRunLock(appName string) (*RunLock, error) {
	if err := s.ensureDirs(appName); err != nil {
		return nil, err
	}
	dir, err := s.locksDir(appName)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "run.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: open lock %s", path)
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		return nil, ErrRunLockHeld
	}
	return &RunLock{file: f}, nil
}

// Release drops the run lock.
func (l *RunLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &windows.Overlapped{})
	_ = l.file.Close()
	l.file = nil
}
