package workspace

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

// WorkspaceLock records who is running a workspace-level orchestration. The
// kernel releases the lock when the holder dies, so a crashed build or
// deploy never leaves a stale lock behind.
type WorkspaceLock struct {
	Operation string    `json:"operation"`
	StartedAt time.Time `json:"started_at"`
	PID       int       `json:"pid"`
}

// AcquireWorkspaceLock takes the workspace-level lock when no other
// orchestration (build or deploy) is in progress for this workspace and
// returns release, which must be called when the operation finishes. The
// lock serializes whole-mesh operations against each other; each service
// still takes its own per-app deploy lock inside, so a workspace operation
// never bypasses single-app safety.
func AcquireWorkspaceLock(name, operation string) (func(), error) {
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name, LockFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "workspace: create lock dir")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "workspace: open lock file")
	}
	if err := lockFileExclusive(f); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			holder := readWorkspaceLockHolder(path)
			if holder == nil {
				holder = &WorkspaceLock{Operation: "unknown", PID: 0}
			}
			return nil, phelixerr.Newf(
				phelixerr.CodeWorkspaceLocked,
				"workspace %q already has a %q operation in progress since %s (pid %d)",
				name, holder.Operation, holder.StartedAt.Format(time.RFC3339), holder.PID,
			)
		}
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "workspace: acquire lock")
	}
	lock := WorkspaceLock{Operation: operation, StartedAt: time.Now(), PID: os.Getpid()}
	writeWorkspaceLockHolder(f, lock)
	release := func() {
		_ = unlockFile(f)
		_ = f.Close()
	}
	return release, nil
}

// writeWorkspaceLockHolder records the holder inside the lock file so a
// rejected acquirer can report actionable details.
func writeWorkspaceLockHolder(f *os.File, lock WorkspaceLock) {
	data, err := json.Marshal(lock)
	if err != nil {
		return
	}
	_, _ = f.Seek(0, 0)
	_ = f.Truncate(0)
	_, _ = f.Write(data)
}

// readWorkspaceLockHolder decodes the holder header from the lock file
// without taking the lock. The result is advisory only.
func readWorkspaceLockHolder(path string) *WorkspaceLock {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil
	}
	var lock WorkspaceLock
	if err := json.Unmarshal(raw, &lock); err != nil || strings.TrimSpace(lock.Operation) == "" {
		return nil
	}
	return &lock
}
