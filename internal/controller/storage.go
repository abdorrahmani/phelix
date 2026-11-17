package controller

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

func MigrateStorage(legacy, directory string) error {
	if filepath.Clean(legacy) == filepath.Clean(directory) {
		return nil
	}
	for _, name := range []string{"master.key", "session.json", "agent-session.json"} {
		if err := copyPersistent(filepath.Join(legacy, name), filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(legacy, "envs"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return fmt.Errorf("legacy environment store contains a non-regular file")
		}
		if err := copyPersistent(filepath.Join(legacy, "envs", entry.Name()), filepath.Join(directory, "envs", entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyPersistent(source, target string) error {
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("persistent source %s is not a regular file", source)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(target)
	if err == nil {
		if !bytes.Equal(current, data) {
			return fmt.Errorf("persistent migration conflict at %s; retain both stores and resolve before startup", target)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".migrate-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(temp.Name(), target); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
