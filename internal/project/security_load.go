package project

import (
	"os"
	"path/filepath"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// LoadSecurityProject reads the registered project's current configuration.
func LoadSecurityProject(dir string) (*Config, error) {
	if !filepath.IsAbs(dir) {
		return nil, phelixerr.New(phelixerr.CodeSecurityRegistrationInvalid, "application registration requires an absolute project directory")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, phelixerr.Wrap(phelixerr.CodeSecurityDirectoryUnavailable, "registered project directory is unavailable", err)
	}
	if !info.IsDir() {
		return nil, phelixerr.New(phelixerr.CodeSecurityRegistrationInvalid, "registered project path is not a directory")
	}
	cfg, err := Load(dir)
	if err != nil {
		code := phelixerr.CodeSecurityConfigLoadFailed
		switch phelixerr.CodeOf(err) {
		case phelixerr.CodeNotFound:
			code = phelixerr.CodeSecurityConfigMissing
		case phelixerr.CodeConfiguration, phelixerr.CodeValidation:
			code = phelixerr.CodeSecurityConfigInvalid
		}
		return nil, phelixerr.Wrap(code, "cannot load registered application's phelix.yaml", err)
	}
	return cfg, nil
}
