package deploy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// DeployLock records an in-flight deploy or rollback so two zero-downtime
// swaps cannot race on the same blue/green slots.
//
// Implementation note: the mutual exclusion primitive is an exclusive,
// non-blocking OS file lock (flock on POSIX, LockFileEx on Windows) on
// ~/.phelix/apps/<AppName>/deploy.lock. The kernel releases it automatically
// when the owning process dies, which makes stale-lock recovery implicit and
// removes the old check-then-write race where two concurrent processes could
// both observe "no lock" in deploy.json and both proceed.
type DeployLock struct {
	Operation string    `json:"operation"`
	StartedAt time.Time `json:"started_at"`
	PID       int       `json:"pid"`
}

// lockHandle owns the open, locked file for the life of a held lock.
type lockHandle struct {
	f *os.File
}

// AcquireDeployLock takes the per-app deploy lock when no other operation
// holds it and returns release, which must be called when the operation
// finishes (success or failure). If another live operation holds the lock the
// call fails with CodeDeployLocked and surfaces the holder's operation and
// start time. Locks held by dead processes are impossible by construction:
// the OS drops them automatically.
func AcquireDeployLock(appName, operation string) (release func(), err error) {
	path, err := lockPath(appName)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: create lock dir")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: open lock file")
	}
	if err := lockFileExclusive(f); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			holder := readLockHolder(path)
			if holder == nil {
				holder = &DeployLock{Operation: "unknown", PID: 0}
			}
			return nil, phelixerr.Newf(
				phelixerr.CodeDeployLocked,
				"deploy: %q already has a %q operation in progress since %s (pid %d)",
				appName, holder.Operation, holder.StartedAt.Format(time.RFC3339), holder.PID,
			)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "deploy: acquire lock")
	}
	h := &lockHandle{f: f}
	lock := DeployLock{Operation: operation, StartedAt: time.Now(), PID: os.Getpid()}
	h.writeHolder(lock)
	mirrorOpLock(appName, &lock)

	release = func() {
		mirrorOpLock(appName, nil)
		_ = unlockFile(h.f)
		_ = h.f.Close()
	}
	return release, nil
}

// writeHolder records who currently holds the lock inside the lock file so a
// rejected acquirer can report actionable details. Best-effort only: losing
// this write never weakens exclusion itself.
func (h *lockHandle) writeHolder(lock DeployLock) {
	data, err := json.Marshal(lock)
	if err != nil {
		return
	}
	_, _ = h.f.Seek(0, 0)
	_, _ = h.f.Write(data)
}

// mirrorOpLock best-effort syncs state.OpLock in deploy.json purely for
// display surfaces (`phelix status` / `phelix deploy`). It never creates a
// state file that does not already exist, so acquiring a lock cannot fabricate
// an empty deploy.json with an unset Mode.
func mirrorOpLock(appName string, lock *DeployLock) {
	state, err := Load(appName)
	if err != nil || state == nil {
		return // no deploy state yet — nothing to annotate
	}
	pidMatches := state.OpLock == nil || lock == nil || state.OpLock.PID == lock.PID
	if !pidMatches {
		// Somebody else updated the advisory field meanwhile; leave it alone.
		return
	}
	state.OpLock = lock
	_ = Store(state)
}

// readLockHolder decodes the holder header from the lock file without taking
// any lock. Callers must treat the result as advisory.
func readLockHolder(path string) *DeployLock {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	var l DeployLock
	if err := json.Unmarshal(data, &l); err != nil || strings.TrimSpace(l.Operation) == "" {
		return nil
	}
	return &l
}

// isPIDAlive returns true if a process with the given PID exists and can
// receive signals. Signal 0 checks existence without actually sending a signal.
// It remains exported at package scope because stale-record diagnostics use it.
func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// TryLoadLock reads the current lock holder for tests and status output. It
// returns nil (and no error) when the app's deploy lock is free — either the
// lock file is absent or no process holds the OS lock on it.
func TryLoadLock(appName string) (*DeployLock, error) {
	path, err := lockPath(appName)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			// No app directory yet — nothing is locked.
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	if err := lockFileExclusive(f); err != nil {
		// Locked: report the recorded holder.
		holder := readLockHolder(path)
		if holder == nil {
			holder = &DeployLock{Operation: "unknown"}
		}
		return holder, nil
	}
	_ = unlockFile(f)
	return nil, nil
}

func lockPath(appName string) (string, error) {
	dir, err := appDataDir(appName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "deploy.lock"), nil
}

// MarshalLockFile writes the human-readable lock header (kept for debugging).
// Exclusion still comes exclusively from the OS file lock.
func MarshalLockFile(appName string, lock DeployLock) error {
	path, err := lockPath(appName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
