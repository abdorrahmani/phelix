package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSecurityConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	content := "name: myapp\n" + body
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSecurityConfig_LoadValid(t *testing.T) {
	dir := writeSecurityConfig(t, `
security:
  vulnerability:
    enabled: true
    scan_interval: 12h
    fail_threshold: medium
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Security.Vulnerability
	if !v.Enabled {
		t.Fatal("enabled must load")
	}
	interval, err := v.ResolvedScanInterval()
	if err != nil || interval != 12*time.Hour {
		t.Fatalf("interval wrong: %v %v", interval, err)
	}
	if v.ResolvedFailThreshold() != "medium" {
		t.Fatalf("threshold wrong: %s", v.ResolvedFailThreshold())
	}
}

func TestSecurityConfig_Defaults(t *testing.T) {
	dir := writeSecurityConfig(t, `
security:
  vulnerability:
    enabled: true
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Security.Vulnerability
	interval, err := v.ResolvedScanInterval()
	if err != nil || interval != ScanIntervalDefault {
		t.Fatalf("default interval wrong: %v %v", interval, err)
	}
	if v.ResolvedFailThreshold() != ScanSeverityDefault {
		t.Fatalf("default threshold wrong: %s", v.ResolvedFailThreshold())
	}
}

func TestSecurityConfig_AbsentSection(t *testing.T) {
	dir := writeSecurityConfig(t, "port: 8080\n")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security != nil {
		t.Fatal("no security section must yield no config")
	}
}

func TestSecurityConfig_Validation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "invalid duration",
			body:    "security:\n  vulnerability:\n    scan_interval: someday\n",
			wantErr: "security.vulnerability.scan_interval",
		},
		{
			name:    "below minimum",
			body:    "security:\n  vulnerability:\n    scan_interval: 1m\n",
			wantErr: "below the 15m0s minimum",
		},
		{
			name:    "non-scalar interval",
			body:    "security:\n  vulnerability:\n    scan_interval: [24h]\n",
			wantErr: "security.vulnerability.scan_interval",
		},
		{
			name:    "unknown threshold",
			body:    "security:\n  vulnerability:\n    fail_threshold: extreme\n",
			wantErr: "security.vulnerability.fail_threshold",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeSecurityConfig(t, tc.body)
			_, err := Load(dir)
			if err == nil {
				t.Fatal("invalid security config must fail")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error must name the yaml path: %v", err)
			}
		})
	}
}

func TestSecurityConfig_ValidationRunsWhenDisabled(t *testing.T) {
	dir := writeSecurityConfig(t, `
security:
  vulnerability:
    enabled: false
    scan_interval: 1m
`)
	if _, err := Load(dir); err == nil {
		t.Fatal("declared values must validate even when scanning is disabled")
	}
}

func TestSecurityConfig_SeverityVocabulary(t *testing.T) {
	for _, valid := range []ScanSeverity{"low", "medium", "high", "critical"} {
		if !valid.Valid() {
			t.Fatalf("%s must be valid", valid)
		}
	}
	for _, invalid := range []ScanSeverity{"", "unknown", "extreme"} {
		if invalid.Valid() {
			t.Fatalf("%q must not be valid", invalid)
		}
	}
}

func TestSecurityConfig_SavePreservesUnrelatedConfiguration(t *testing.T) {
	dir := t.TempDir()
	content := `# Phelix project configuration.
name: myapp
port: 8080

matrix:
  go_versions: ["1.24"]

webhook:
  enabled: false
  branch: main
  secret_env: PHELIX_WEBHOOK_SECRET
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	sc := &SecurityConfig{Vulnerability: &VulnerabilityConfig{
		Enabled:       true,
		ScanInterval:  ScanInterval("12h"),
		FailThreshold: ScanSeverity("medium"),
	}}
	if err := SaveSecurityConfig(dir, sc); err != nil {
		t.Fatalf("SaveSecurityConfig: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, must := range []string{"name: myapp", "port: 8080", "go_versions", "branch: main", "secret_env: PHELIX_WEBHOOK_SECRET", "# Phelix project configuration."} {
		if !strings.Contains(text, must) {
			t.Fatalf("unrelated configuration must survive: missing %q in\n%s", must, text)
		}
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	v := cfg.Security.Vulnerability
	if !v.Enabled || v.ResolvedFailThreshold() != "medium" {
		t.Fatalf("security section round trip: %+v", v)
	}
	interval, _ := v.ResolvedScanInterval()
	if interval != 12*time.Hour {
		t.Fatalf("interval round trip: %v", interval)
	}
}

func TestSecurityConfig_SaveRejectsInvalidBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	content := "name: myapp\nport: 8080\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := &SecurityConfig{Vulnerability: &VulnerabilityConfig{
		Enabled:      true,
		ScanInterval: ScanInterval("1m"),
	}}
	if err := SaveSecurityConfig(dir, sc); err == nil {
		t.Fatal("an invalid interval must fail before writing")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	if strings.Contains(string(raw), "security") {
		t.Fatalf("a rejected update must not touch the file:\n%s", raw)
	}
}

func TestSecurityConfig_SaveCreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	sc := &SecurityConfig{Vulnerability: &VulnerabilityConfig{Enabled: false}}
	if err := SaveSecurityConfig(dir, sc); err != nil {
		t.Fatalf("SaveSecurityConfig: %v", err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security == nil || cfg.Security.Vulnerability == nil {
		t.Fatal("the created file must carry the security section")
	}
}
