package project

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const MaxConfigBytes = 512 << 10

var ErrConfigConflict = errors.New("configuration changed since it was loaded")

type ConfigContent struct {
	Exists    bool
	Content   []byte
	Revision  string
	UpdatedAt time.Time
}

// ContentRevision identifies the exact contents of a configuration.
func ContentRevision(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ReadContent reads the configuration and its metadata without changing it.
func ReadContent(dir string) (ConfigContent, error) {
	return readProjectContent(dir, MaxConfigBytes)
}

var errConfigReadChanged = errors.New("phelix.yaml changed while it was being opened")

func readProjectContent(dir string, limit int64) (ConfigContent, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ConfigContent{}, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot open application directory", err)
	}
	defer root.Close()
	return readConfigRoot(root, limit)
}

func readConfigRoot(root *os.Root, limit int64) (ConfigContent, error) {
	for attempt := 0; attempt < 3; attempt++ {
		content, err := readConfigRootOnce(root, limit)
		if !errors.Is(err, errConfigReadChanged) {
			return content, err
		}
	}
	return ConfigContent{}, phelixerr.Wrap(phelixerr.CodeFilesystem, "phelix.yaml changed repeatedly; retry the request", errConfigReadChanged)
}

func readConfigRootOnce(root *os.Root, limit int64) (ConfigContent, error) {
	info, err := root.Lstat(FileName)
	if err != nil {
		if os.IsNotExist(err) {
			return ConfigContent{}, nil
		}
		return ConfigContent{}, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot inspect phelix.yaml", err)
	}
	if !info.Mode().IsRegular() {
		return ConfigContent{}, phelixerr.New(phelixerr.CodeFilesystem, "phelix.yaml must be a regular file, not a symbolic link")
	}
	if limit > 0 && info.Size() > limit {
		return ConfigContent{}, configSizeError()
	}
	f, err := root.Open(FileName)
	if err != nil {
		return ConfigContent{}, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot read phelix.yaml", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return ConfigContent{}, errConfigReadChanged
	}
	var reader io.Reader = f
	if limit > 0 {
		reader = io.LimitReader(f, limit+1)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return ConfigContent{}, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot read phelix.yaml", err)
	}
	if limit > 0 && int64(len(raw)) > limit {
		return ConfigContent{}, configSizeError()
	}
	return ConfigContent{Exists: true, Content: raw, Revision: ContentRevision(raw), UpdatedAt: opened.ModTime().UTC()}, nil
}

func configSizeError() error {
	return phelixerr.Newf(phelixerr.CodeConfiguration, "phelix.yaml exceeds the %d byte document limit", MaxConfigBytes)
}

// ValidateContent uses the same configuration rules as the local CLI.
func ValidateContent(dir string, raw []byte) (*Config, error) {
	if len(raw) > MaxConfigBytes {
		return nil, configSizeError()
	}
	if err := validateRemoteYAML(raw); err != nil {
		return nil, err
	}
	cfg, err := decodeConfig(raw, FileName, false)
	if err != nil {
		return nil, err
	}
	return cfg, validateConfig(dir, cfg)
}

type SaveOptions struct {
	ExpectedRevision string
	MustNotExist     bool
	AccessPolicy     ConfigAccessPolicy
}

type SaveResult struct {
	ConfigContent
	Changed bool
}

// SaveContent validates and saves a document while checking its loaded revision.
func SaveContent(dir string, raw []byte, opts SaveOptions) (SaveResult, error) {
	cfg, err := ValidateContent(dir, raw)
	if err != nil {
		return SaveResult{}, err
	}
	release, err := acquireConfigLock(dir)
	if err != nil {
		return SaveResult{}, err
	}
	defer release()
	current, err := ReadContent(dir)
	if err != nil {
		return SaveResult{}, err
	}
	if err := CheckConfigReadAccess(current.Content, opts.AccessPolicy.ReadDeniedPaths); err != nil {
		return SaveResult{}, err
	}
	if err := CheckConfigReadAccess(raw, opts.AccessPolicy.ReadDeniedPaths); err != nil {
		return SaveResult{}, err
	}
	newRevision := ContentRevision(raw)
	if current.Exists && current.Revision == newRevision {
		return SaveResult{ConfigContent: current}, nil
	}
	if opts.MustNotExist && current.Exists || !opts.MustNotExist && current.Exists && current.Revision != opts.ExpectedRevision {
		return SaveResult{ConfigContent: current}, phelixerr.Wrap(phelixerr.CodeConfiguration, "phelix.yaml changed on disk; reload its latest version", ErrConfigConflict)
	}
	if !opts.MustNotExist && !current.Exists {
		return SaveResult{}, phelixerr.New(phelixerr.CodeNotFound, "phelix.yaml no longer exists; reload before creating it")
	}
	baseline := current.Content
	if !current.Exists {
		baseline = opts.AccessPolicy.InitialContent
	}
	if err := checkConfigWriteAccess(baseline, raw, opts.AccessPolicy.WriteDeniedPaths); err != nil {
		return SaveResult{}, err
	}
	if err := validateRemotePolicy(current.Content, cfg); err != nil {
		return SaveResult{}, err
	}
	if err := writeConfigAtomic(filepath.Join(dir, FileName), raw, current); err != nil {
		if errors.Is(err, ErrConfigConflict) {
			latest, _ := ReadContent(dir)
			if accessErr := CheckConfigReadAccess(latest.Content, opts.AccessPolicy.ReadDeniedPaths); accessErr != nil {
				return SaveResult{}, accessErr
			}
			return SaveResult{ConfigContent: latest}, err
		}
		return SaveResult{}, err
	}
	saved, err := ReadContent(dir)
	if accessErr := CheckConfigReadAccess(saved.Content, opts.AccessPolicy.ReadDeniedPaths); accessErr != nil {
		return SaveResult{}, accessErr
	}
	return SaveResult{ConfigContent: saved, Changed: true}, err
}

func writeConfigAtomic(path string, data []byte, expected ConfigContent) error {
	dir := filepath.Dir(path)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot open configuration directory", err)
	}
	defer root.Close()
	current, err := readConfigRoot(root, 0)
	if err != nil {
		return err
	}
	if current.Exists != expected.Exists || current.Revision != expected.Revision {
		return ErrConfigConflict
	}
	if current.Exists && bytes.Equal(current.Content, data) {
		return nil
	}
	owner, err := root.Stat(".")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot inspect configuration ownership", err)
	}
	mode := os.FileMode(0o644)
	if info, err := root.Lstat(FileName); err == nil {
		if !info.Mode().IsRegular() {
			return phelixerr.New(phelixerr.CodeFilesystem, "phelix.yaml must be a regular file")
		}
		mode = info.Mode().Perm()
		owner = info
	} else if !os.IsNotExist(err) {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot inspect configuration permissions", err)
	}
	temp := ".phelix-config-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot create temporary configuration", err)
	}
	defer root.Remove(temp)
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot preserve configuration permissions", err)
	}
	if err := preserveConfigOwner(f, owner); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot preserve configuration ownership", err)
	}
	if _, err := f.Write(data); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot write configuration", err)
	}
	if err := f.Sync(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot sync configuration", err)
	}
	if err := f.Close(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot close configuration", err)
	}
	latest, err := readConfigRoot(root, 0)
	if err != nil {
		return err
	}
	if latest.Exists != expected.Exists || latest.Revision != expected.Revision {
		return ErrConfigConflict
	}
	if err := root.Rename(temp, FileName); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot replace phelix.yaml", err)
	}
	directory, err := root.Open(".")
	if err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "configuration saved but directory could not be opened", err)
	}
	defer directory.Close()
	if err := syncConfigDirectory(directory); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "configuration saved but directory could not be synced", err)
	}
	return nil
}

func openConfigLock(dir string) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot open application directory", err)
	}
	defer root.Close()
	const name = ".phelix-config.lock"
	info, err := root.Lstat(name)
	if err != nil && !os.IsNotExist(err) {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot inspect configuration lock", err)
	}
	if info != nil && !info.Mode().IsRegular() {
		return nil, phelixerr.New(phelixerr.CodeFilesystem, "configuration lock must be a regular file")
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR|configNoFollow, 0o600)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot open configuration lock", err)
	}
	actual, statErr := root.Lstat(name)
	opened, openedErr := f.Stat()
	if statErr != nil || openedErr != nil || !actual.Mode().IsRegular() || !os.SameFile(actual, opened) {
		f.Close()
		return nil, phelixerr.New(phelixerr.CodeFilesystem, "configuration lock changed while it was being opened")
	}
	owner, err := root.Stat(".")
	if err != nil {
		f.Close()
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot inspect configuration lock ownership", err)
	}
	if err := preserveConfigOwner(f, owner); err != nil {
		f.Close()
		return nil, phelixerr.Wrap(phelixerr.CodeFilesystem, "cannot preserve configuration lock ownership", err)
	}
	return f, nil
}

// AppendContent appends documentation while preserving concurrent configuration changes.
func AppendContent(dir string, text string) error {
	release, err := acquireConfigLock(dir)
	if err != nil {
		return err
	}
	defer release()
	content, err := readProjectContent(dir, 0)
	if err != nil {
		return err
	}
	if !content.Exists {
		return phelixerr.New(phelixerr.CodeNotFound, "phelix.yaml does not exist")
	}
	return writeConfigAtomic(filepath.Join(dir, FileName), append(content.Content, text...), content)
}
