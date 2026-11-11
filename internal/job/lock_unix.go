//go:build !windows

package job

import (
	"os"
	"path/filepath"
	"syscall"

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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: open lock %s", path)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrRunLockHeld
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "job: lock %s", path)
	}
	return &RunLock{file: f}, nil
}

// Release drops the run lock.
func (l *RunLock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
