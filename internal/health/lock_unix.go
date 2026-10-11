//go:build !windows

package health

import (
	"os"
	"syscall"
)

// lockFileExclusive takes a BLOCKING exclusive BSD flock on f. Unlike deploy's
// non-blocking deploy.lock (which rejects a second deploy), health config
// writers should queue behind each other, not fail: a CLI `health add` racing
// the monitor daemon's write simply waits its turn. The kernel releases the
// lock automatically if the process dies, so stale locks are impossible.
//
// Sibling of internal/deploy/lock_unix.go; duplicated rather than shared because
// internal/deploy imports internal/health (a shared helper would cycle).
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases the exclusive lock taken by lockFileExclusive.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
