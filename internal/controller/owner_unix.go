//go:build !windows

package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

type Owner struct {
	file *os.File
	once sync.Once
}

func Acquire(directory string) (*Owner, error) {
	if !filepath.IsAbs(directory) {
		return nil, fmt.Errorf("controller data directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(filepath.Join(directory, "controller.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "controller.lock")
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another Phelix Controller owns %s: %w", directory, err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("controller lock is not a regular file")
	}
	return &Owner{file: file}, nil
}

func (o *Owner) Close() error {
	var err error
	o.once.Do(func() { err = o.file.Close() })
	return err
}
