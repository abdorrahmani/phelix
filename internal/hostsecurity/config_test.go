package hostsecurity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateScanInterval(t *testing.T) {
	for raw, want := range map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "2h30m": 150 * time.Minute} {
		got, err := ValidateScanInterval(raw)
		if err != nil || got != want {
			t.Fatalf("%s: %d %v", raw, got, err)
		}
	}
	for _, raw := range []string{"30m", "59m", "abc", "", "0h", "-1h"} {
		if _, err := ValidateScanInterval(raw); err == nil {
			t.Fatalf("accepted invalid interval %q", raw)
		}
	}
}

func TestHostConfigPersistence(t *testing.T) {
	dataDir := t.TempDir()
	cfg, err := LoadHostConfig(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reporting || cfg.ScanInterval != DefaultHostScanIntervalValue {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if HostReportingEnabled(dataDir) {
		t.Fatal("reporting consented without a config file")
	}
	if HostScanInterval(dataDir) != DefaultHostScanInterval {
		t.Fatalf("scan interval default not applied: %v", HostScanInterval(dataDir))
	}

	cfg.Reporting = true
	cfg.ScanInterval = "6h"
	if err := SaveHostConfig(dataDir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadHostConfig(dataDir)
	if err != nil || !loaded.Reporting || loaded.ScanInterval != "6h" {
		t.Fatalf("round trip failed: %+v %v", loaded, err)
	}
	if !HostReportingEnabled(dataDir) {
		t.Fatal("consent not persisted")
	}
	info, err := os.Stat(HostConfigPath(dataDir))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config file mode not restrictive: %v %v", info, err)
	}

	if err := SaveHostConfig(dataDir, HostConfig{Reporting: true, ScanInterval: "30m"}); err == nil {
		t.Fatal("saved an interval below the minimum")
	}
	loaded, err = LoadHostConfig(dataDir)
	if err != nil || !loaded.Reporting || loaded.ScanInterval != "6h" {
		t.Fatalf("failed save changed the file: %+v %v", loaded, err)
	}
	if err := os.WriteFile(HostConfigPath(dataDir), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHostConfig(dataDir); err == nil {
		t.Fatal("malformed config accepted")
	}
	if HostReportingEnabled(dataDir) {
		t.Fatal("malformed config read as consent")
	}
	if err := os.WriteFile(HostConfigPath(dataDir), []byte(`{"reporting":true,"scan_interval":"20m"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHostConfig(dataDir); err == nil {
		t.Fatal("invalid interval accepted on load")
	}
	if HostReportingEnabled(dataDir) {
		t.Fatal("invalid config read as consent")
	}
	if HostScanInterval(dataDir) != DefaultHostScanInterval {
		t.Fatal("invalid config did not fall back to the default cadence")
	}
}

func TestHostConfigPathUnderDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "nested")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if got, want := HostConfigPath(dataDir), filepath.Join(dataDir, "host-security", "config.json"); got != want {
		t.Fatalf("path %s, want %s", got, want)
	}
}
