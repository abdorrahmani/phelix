//go:build !windows

package session

import (
	"os"
	"syscall"
)

// lockFileBlocking takes an exclusive BSD flock on f, BLOCKING until it is
// acquired. Unlike the deploy lock (which is non-blocking so a second deploy
// fails fast), session updates must serialize rather than fail: concurrent
// writers queue on the lock and every update lands, so no reference is lost.
// The kernel releases the lock automatically if this process dies, so a stale
// lock is impossible by construction.
func lockFileBlocking(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases the exclusive lock taken by lockFileBlocking.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
