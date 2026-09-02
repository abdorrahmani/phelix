package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadValidFullConfig(t *testing.T) {
	dir := write(t, `
name: concurrency-lab-api
port: 3000

health:
  endpoints:
    - name: default
      path: /health
      interval: 10s
      retries: 3
      mode: auto
    - name: readiness
      path: /ready
      interval: 5s
      retries: 5
      mode: http

deploy:
  strategy: blue-green
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "concurrency-lab-api" || cfg.Port != 3000 {
		t.Errorf("name/port = %q/%d", cfg.Name, cfg.Port)
	}
	if cfg.Health == nil || len(cfg.Health.Endpoints) != 2 {
		t.Fatalf("endpoints = %+v, want 2", cfg.Health)
	}
	if cfg.Health.Endpoints[1].Name != "readiness" || cfg.Health.Endpoints[1].Path != "/ready" {
		t.Errorf("second endpoint = %+v", cfg.Health.Endpoints[1])
	}
	if cfg.Deploy == nil || cfg.Deploy.Strategy != StrategyBlueGreen {
		t.Errorf("deploy = %+v", cfg.Deploy)
	}
}

func TestLoadPartialConfigs(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"name only", "name: api\n"},
		{"port only", "port: 3000\n"},
		{"empty file", ""},
		{"rolling with replicas", "name: api\ndeploy:\n  strategy: rolling\n  replicas: 3\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(write(t, tc.content)); err != nil {
				t.Errorf("Load rejected valid partial config: %v", err)
			}
		})
	}
}

func TestLoadInvalidConfigs(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"invalid strategy", "name: api\ndeploy:\n  strategy: foobar\n", `invalid deploy.strategy "foobar"`},
		{"replicas without rolling", "name: api\ndeploy:\n  strategy: classic\n  replicas: 2\n", "deploy.replicas requires"},
		{"negative replicas", "name: api\ndeploy:\n  strategy: rolling\n  replicas: -1\n", "invalid deploy.replicas"},
		{"duplicate endpoint names", "name: api\nhealth:\n  endpoints:\n    - name: x\n      path: /a\n    - name: x\n      path: /b\n", "duplicate health endpoint name"},
		{"missing endpoint name", "name: api\nhealth:\n  endpoints:\n    - path: /a\n", "missing required field: name"},
		{"path without slash", "name: api\nhealth:\n  endpoints:\n    - name: x\n      path: health\n", "invalid path"},
		{"bad interval", "name: api\nhealth:\n  endpoints:\n    - name: x\n      path: /a\n      interval: fast\n", "invalid interval"},
		{"bad mode", "name: api\nhealth:\n  endpoints:\n    - name: x\n      path: /a\n      mode: tcp\n", "invalid mode"},
		{"negative retries", "name: api\nhealth:\n  endpoints:\n    - name: x\n      path: /a\n      retries: -2\n", "invalid retries"},
		{"bad port", "name: api\nport: 70000\n", "invalid port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(write(t, tc.content))
			if err == nil {
				t.Fatalf("invalid config accepted:\n%s", tc.content)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadMalformedYAML(t *testing.T) {
	if _, err := Load(write(t, "name: [unclosed\n")); err == nil {
		t.Error("malformed yaml accepted")
	}
}

func TestSaveLoadRoundTripFullConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Name: "api",
		Port: 4000,
		Health: &HealthConfig{Endpoints: []HealthEndpoint{
			{Name: "default", Path: "/health", Interval: "10s", Retries: 3, Mode: "auto"},
		}},
		Deploy: &DeployConfig{Strategy: StrategyRolling, Replicas: 3},
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Deploy == nil || got.Deploy.Strategy != StrategyRolling || got.Deploy.Replicas != 3 {
		t.Errorf("deploy = %+v, want rolling/3", got.Deploy)
	}
	if got.Health == nil || len(got.Health.Endpoints) != 1 || got.Health.Endpoints[0].Path != "/health" {
		t.Errorf("health = %+v", got.Health)
	}
}
