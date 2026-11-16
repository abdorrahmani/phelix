//go:build !windows

package project

import (
	"os"
	"syscall"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const configNoFollow = syscall.O_NOFOLLOW

func acquireConfigLock(dir string) (func(), error) {
	f, err := openConfigLock(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot lock configuration", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func syncConfigDirectory(dir *os.File) error { return dir.Sync() }

func preserveConfigOwner(file *os.File, owner os.FileInfo) error {
	expected, ok := owner.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	current, ok := info.Sys().(*syscall.Stat_t)
	if !ok || current.Uid == expected.Uid && current.Gid == expected.Gid {
		return nil
	}
	return file.Chown(int(expected.Uid), int(expected.Gid))
}
