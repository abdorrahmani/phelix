//go:build windows

package secrets

import (
	"os"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// acquireStoreLock opens the app's lock file on platforms without BSD flock.
// Serialization between processes is provided by the atomic rename of the
// store itself on these platforms; concurrent readers and writers from the
// same process are covered by the callers' own discipline.
func acquireStoreLock(path string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeFilesystem, err, "secrets: open lock %s", path)
	}
	return f, nil
}

// releaseStoreLock closes the lock file opened by acquireStoreLock.
func releaseStoreLock(f *os.File) error {
	return f.Close()
}
