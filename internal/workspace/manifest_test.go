package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func writeManifest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestLoad_MissingFileIsNotFound(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load on missing manifest should fail")
	}
}

func TestLoad_MalformedYamlFails(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "services: [broken")
	if _, err := Load(dir); err == nil {
		t.Fatalf("Load on malformed manifest should fail")
	}
}

func TestLoad_ValidManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `
name: mesh
services:
  auth: { path: ./services/auth, port: 8081, strategy: blue-green }
  orders: { path: ./services/orders, port: 8082, depends_on: [auth] }
  gateway: { path: ./gateway, port: 8080, depends_on: [auth, orders] }
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Name != "mesh" {
		t.Fatalf("Name = %q, want mesh", m.Name)
	}
	if len(m.Services) != 3 {
		t.Fatalf("len(Services) = %d, want 3", len(m.Services))
	}
	orders := m.Services["orders"]
	if orders == nil || orders.Path != "./services/orders" || orders.Port != 8082 {
		t.Fatalf("orders spec = %+v", orders)
	}
	if len(orders.DependsOn) != 1 || orders.DependsOn[0] != "auth" {
		t.Fatalf("orders depends_on = %v, want [auth]", orders.DependsOn)
	}
	if m.Services["gateway"].Strategy != "" {
		t.Fatalf("gateway strategy = %q, want empty", m.Services["gateway"].Strategy)
	}
}

func TestManifestValidate_Table(t *testing.T) {
	valid := `services:
  auth: { path: ./auth, port: 8081 }
  orders: { path: ./orders, depends_on: [auth] }
`
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"valid minimal", valid, ""},
		{
			"missing path",
			"services:\n  auth: { port: 8081 }\n",
			"missing path",
		},
		{
			"bad port range",
			"services:\n  auth: { path: ./auth, port: 99999 }\n",
			"invalid port",
		},
		{
			"unknown strategy",
			"services:\n  auth: { path: ./auth, strategy: canary }\n",
			"invalid strategy",
		},
		{
			"replicas without rolling",
			"services:\n  auth: { path: ./auth, strategy: blue-green, replicas: 2 }\n",
			"replicas without the rolling strategy",
		},
		{
			"self dependency",
			"services:\n  auth: { path: ./auth, depends_on: [auth] }\n",
			"depends on itself",
		},
		{
			"unknown dependency",
			"services:\n  auth: { path: ./auth, depends_on: [missing] }\n",
			"unknown service",
		},
		{
			"invalid service name",
			"services:\n  \"bad name\": { path: ./auth }\n",
			"invalid service name",
		},
		{
			"nil service entry",
			"services:\n  auth:\n",
			"has no configuration",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseOnly(t, tc.content)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			err = m.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func parseOnly(t *testing.T, content string) (*Manifest, error) {
	t.Helper()
	var m Manifest
	if err := yaml.Unmarshal([]byte(content), &m); err != nil {
		return nil, err
	}
	return &m, nil
}
