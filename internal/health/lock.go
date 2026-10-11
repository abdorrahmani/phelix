package health

import (
	"os"
	"path/filepath"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// healthLockName is the single health-config write lock, kept in the apps base
// dir (~/.phelix/apps). A dot-prefixed file so loadAllConfigs — which only
// descends into subdirectories — never mistakes it for an app.
const healthLockName = ".health-config.lock"

// acquireHealthLock takes the health-config write lock and returns a release
// func. It serializes read-modify-write cycles on health.json across processes
// (CLI health commands, `phelix build`, the monitor daemon), which an
// in-process mutex cannot do. It is the OS file lock ~/.phelix/apps/<lock>,
// mirroring deploy's deploy.lock; the kernel drops it on death, so stale locks
// are impossible.
//
// It is one global lock rather than per-app: health-config writes are rare and
// sub-millisecond, and a single lock avoids the app-folder-name ambiguity a
// per-app lock would have (the folder name is only known after reading the
// config the lock is meant to protect). The lock is BLOCKING — a second writer
// waits rather than failing (see lockFileExclusive). Callers take it BEFORE
// cm.mu, never the reverse, so there is no lock-ordering inversion with
// SaveConfig/Reload.
func acquireHealthLock(basePath string) (func(), error) {
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to create health lock dir %s", basePath)
	}
	path := filepath.Join(basePath, healthLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to open health lock %s", path)
	}
	if err := lockFileExclusive(f); err != nil {
		f.Close()
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "failed to acquire health lock %s", path)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
