package deployanalysis

import (
	"os"
	"path/filepath"

	"github.com/abdorrahmani/phelix/internal/deploy"
)

// withStoreLock serializes every read-modify-write of one app's analysis
// files. The lock is a separate file so a crash never leaves the data file
// locked, and the kernel releases it automatically if the process dies.
func withStoreLock(appName, target string, fn func() error) error {
	dir, err := deploy.AppDataDir(appName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, target+".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockFileExclusive(f); err != nil {
		return err
	}
	defer func() { _ = unlockFile(f) }()
	return fn()
}
