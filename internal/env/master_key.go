package env

import (
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// GetMasterKey loads the existing key or creates one without replacing another writer's key.
func GetMasterKey() ([]byte, error) {
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}
	path := filepath.Join(home, ".phelix", "master.key")
	key, err := readMasterKey(path)
	if !errors.Is(err, os.ErrNotExist) {
		return key, err
	}
	key, err = GenerateMasterKey()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot create master key directory", err)
	}
	f, err := os.CreateTemp(dir, ".master-key-*")
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot create master key", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.WriteString(hex.EncodeToString(key)); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot write master key", err)
	}
	if err := f.Sync(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot sync master key", err)
	}
	if err := f.Close(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot close master key", err)
	}
	if err := os.Link(f.Name(), path); err != nil {
		if os.IsExist(err) {
			return readMasterKey(path)
		}
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot save master key", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot open master key directory", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot sync master key directory", err)
	}
	return key, nil
}

// readMasterKey refuses damaged keys instead of replacing credentials already in use.
func readMasterKey(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot read master key", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot inspect master key", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, phelixerr.New(phelixerr.CodeEncryption, "master key must be a regular file with mode 0600")
	}
	data, err := io.ReadAll(io.LimitReader(f, 67))
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeEncryption, "cannot read master key", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != 32 {
		return nil, phelixerr.New(phelixerr.CodeEncryption, "master key is invalid; restore the original key")
	}
	return key, nil
}
