//go:build windows

package project

import (
	"os"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"golang.org/x/sys/windows"
)

const configNoFollow = 0

func acquireConfigLock(dir string) (func(), error) {
	f, err := openConfigLock(dir)
	if err != nil {
		return nil, err
	}
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		f.Close()
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot lock configuration", err)
	}
	return func() {
		windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
		f.Close()
	}, nil
}

func syncConfigDirectory(dir *os.File) error { return nil }

func preserveConfigOwner(file *os.File, owner os.FileInfo) error { return nil }
