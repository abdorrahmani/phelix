//go:build windows

package app

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockStateFile serializes state updates across CLI and daemon processes.
func lockStateFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{}); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// unlockStateFile releases the persistent state lock.
func unlockStateFile(file *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
	_ = file.Close()
}
