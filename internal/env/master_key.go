package env

import (
	"encoding/hex"
	"encoding/json"
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
	dataDir := os.Getenv("PHELIX_DATA_DIR")
	if dataDir == "" {
		dataDir = filepath.Dir(path)
		if _, err := os.Stat("/.dockerenv"); err == nil {
			dataDir = "/var/lib/phelix"
		}
	}
	for _, location := range []struct {
		path string
		apps bool
	}{
		{filepath.Join(dataDir, "secrets"), false},
		{filepath.Join(home, ".phelix", "envs"), false},
		{filepath.Join(home, ".phelix", "apps"), true},
	} {
		if err := inspectEncryptedStores(location.path, location.apps); err != nil {
			return nil, err
		}
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

// ReadMasterKey reads an existing key without creating a replacement.
func ReadMasterKey() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, phelixerr.New(phelixerr.CodeEncryption, "cannot locate master key")
	}
	return readMasterKey(filepath.Join(home, ".phelix", "master.key"))
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

// inspectEncryptedStores refuses replacement keys when existing data cannot be checked.
func inspectEncryptedStores(dir string, apps bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return phelixerr.New(phelixerr.CodeEncryption, "cannot inspect existing encrypted stores")
	}
	for _, entry := range entries {
		if apps {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				if err := inspectEncryptedStores(filepath.Join(dir, entry.Name(), "env"), false); err != nil {
					return err
				}
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") && !strings.HasSuffix(entry.Name(), ".enc") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		var stored struct {
			Secrets map[string]json.RawMessage
			Entries map[string]json.RawMessage
		}
		if err != nil || json.Unmarshal(data, &stored) != nil || len(stored.Secrets) > 0 || len(stored.Entries) > 0 {
			return phelixerr.New(phelixerr.CodeEncryption, "master key is missing; restore the original key before changing encrypted data")
		}
	}
	return nil
}
