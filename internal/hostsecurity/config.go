package hostsecurity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// DefaultHostScanInterval is the consented scan cadence when unset.
const DefaultHostScanInterval = 24 * time.Hour

// DefaultHostScanIntervalValue is the persisted default cadence string.
const DefaultHostScanIntervalValue = "24h"

// MinHostScanInterval bounds how often host scans may run.
const MinHostScanInterval = time.Hour

// HostConfig is the local-only consent configuration for host security
// scans and reporting. It stays on this host: there is no remote toggle and
// the file is never transmitted.
type HostConfig struct {
	Reporting    bool   `json:"reporting"`
	ScanInterval string `json:"scan_interval"`
}

// HostConfigPath returns the persisted consent configuration path under the
// data directory.
func HostConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "host-security", "config.json")
}

// ValidateScanInterval parses a Go duration string and enforces the minimum
// cadence.
func ValidateScanInterval(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("scan interval must be a Go duration, e.g. 24h")
	}
	if d < MinHostScanInterval {
		return 0, errors.New("scan interval must be at least 1h")
	}
	return d, nil
}

// DefaultHostConfig returns the consent defaults: reporting off, daily scans.
func DefaultHostConfig() HostConfig {
	return HostConfig{Reporting: false, ScanInterval: DefaultHostScanIntervalValue}
}

// LoadHostConfig reads the consent configuration, falling back to the
// defaults when no file exists. A malformed or invalid file is an error so
// fail-closed callers never scan or report on unknown consent.
func LoadHostConfig(dataDir string) (HostConfig, error) {
	cfg := DefaultHostConfig()
	raw, err := os.ReadFile(HostConfigPath(dataDir))
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DefaultHostConfig(), errors.New("host security configuration is malformed")
	}
	if _, err := ValidateScanInterval(cfg.ScanInterval); err != nil {
		return DefaultHostConfig(), err
	}
	return cfg, nil
}

// SaveHostConfig validates and persists the consent configuration with
// restrictive file permissions. The write is atomic (temp file, fsync,
// rename) like the other persisted host-security files, so a crash can
// never leave a truncated consent file that would fail closed readers.
func SaveHostConfig(dataDir string, cfg HostConfig) error {
	if _, err := ValidateScanInterval(cfg.ScanInterval); err != nil {
		return err
	}
	path := HostConfigPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "config-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(name, 0600)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// HostReportingEnabled reports whether the operator consented to host
// security reporting. Read errors mean no consent so the remote layer stays
// fail-closed.
func HostReportingEnabled(dataDir string) bool {
	cfg, err := LoadHostConfig(dataDir)
	if err != nil {
		return false
	}
	return cfg.Reporting
}

// HostScanInterval returns the consented scan cadence, falling back to the
// default when the configuration cannot be read.
func HostScanInterval(dataDir string) time.Duration {
	cfg, err := LoadHostConfig(dataDir)
	if err != nil {
		return DefaultHostScanInterval
	}
	d, err := ValidateScanInterval(cfg.ScanInterval)
	if err != nil {
		return DefaultHostScanInterval
	}
	return d
}
