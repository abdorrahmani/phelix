//go:build !windows

package ops

import (
	"os"
	"syscall"
)

// lockFileBlocking takes an exclusive BSD flock on f, BLOCKING until it is
// acquired. The request-key ledger's begin/complete critical sections take it
// so two processes starting near-simultaneously cannot both read the ledger
// before either persists its in_progress entry (which would let the same key
// execute twice). It mirrors internal/session/lock_unix.go: updates serialize
// rather than fail, and the kernel releases the lock automatically if this
// process dies, so a stale lock is impossible by construction.
func lockFileBlocking(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases the exclusive lock taken by lockFileBlocking.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
