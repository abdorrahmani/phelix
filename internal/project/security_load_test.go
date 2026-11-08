package project

import (
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestLoadSecurityProject(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		code          phelixerr.Code
		reporting     bool
	}{
		{name: "enabled", content: "name: app\nsecurity:\n  vulnerability:\n    reporting: true\n", reporting: true},
		{name: "disabled", content: "security:\n  vulnerability:\n    reporting: false\n"},
		{name: "default disabled", content: "name: app\n"},
		{name: "invalid yaml", content: "security: [", code: phelixerr.CodeSecurityConfigInvalid},
		{name: "invalid settings", content: "security:\n  vulnerability:\n    reporting: true\n    scan_interval: 1m\n", code: phelixerr.CodeSecurityConfigInvalid},
		{name: "wrong identity", content: "name: other\nsecurity:\n  vulnerability:\n    reporting: true\n", code: phelixerr.CodeSecurityRegistrationInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadSecurityProject(dir, "app")
			if tc.code != "" {
				if phelixerr.CodeOf(err) != tc.code {
					t.Fatalf("error = %v, want %s", err, tc.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			reporting := cfg.Security != nil && cfg.Security.Vulnerability != nil && cfg.Security.Vulnerability.Reporting
			if reporting != tc.reporting {
				t.Fatalf("reporting = %v", reporting)
			}
		})
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		dir  string
		code phelixerr.Code
	}{
		{"", phelixerr.CodeSecurityRegistrationInvalid},
		{"relative", phelixerr.CodeSecurityRegistrationInvalid},
		{file, phelixerr.CodeSecurityRegistrationInvalid},
		{filepath.Join(dir, "missing"), phelixerr.CodeSecurityDirectoryUnavailable},
		{dir, phelixerr.CodeSecurityConfigMissing},
	} {
		if _, err := LoadSecurityProject(tc.dir, "app"); phelixerr.CodeOf(err) != tc.code {
			t.Fatalf("%q: %v, want %s", tc.dir, err, tc.code)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, FileName), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSecurityProject(dir, "app"); phelixerr.CodeOf(err) != phelixerr.CodeSecurityConfigLoadFailed {
		t.Fatalf("read failure = %v", err)
	}
}

func TestSaveSecurityConfigPreservesLocalConsent(t *testing.T) {
	for _, reporting := range []string{"true", "false"} {
		dir := t.TempDir()
		content := "security:\n  vulnerability:\n    reporting: " + reporting + "\n    custom: preserved\n"
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := SaveSecurityConfig(dir, &SecurityConfig{Vulnerability: &VulnerabilityConfig{Enabled: true, Reporting: reporting != "true"}}); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Security.Vulnerability.Reporting != (reporting == "true") {
			t.Fatal("update changed local consent")
		}
	}
	dir := t.TempDir()
	if err := SaveSecurityConfig(dir, &SecurityConfig{Vulnerability: &VulnerabilityConfig{Reporting: true}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil || cfg.Security.Vulnerability.Reporting {
		t.Fatalf("update granted consent: %v", err)
	}
}
