package workspace

import (
	"os"
	"path/filepath"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

const exampleManifest = `
version: 1
name: my-mesh
root: .
discovery:
  go_work: on
  include: ["services/*"]
  exclude: ["services/legacy"]
defaults:
  strategy: blue-green
  runtime: native
  replicas: 2
  health: { mode: http, path: /healthz }
services:
  auth:
    path: ./services/auth/
    port: 8081
    strategy: blue-green
    health: { mode: http, path: /healthz }
    build: { args: ["-tags=prod"], env: { FOO: bar } }
  orders:
    path: ./services/orders
    port: 8082
    depends_on: [auth]
  gateway:
    path: ./gateway
    port: 8080
    depends_on: [auth, orders]
`

func TestParseManifestExample(t *testing.T) {
	m, err := ParseManifest([]byte(exampleManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Version != 1 || m.Name != "my-mesh" || m.Root != "." {
		t.Errorf("header = {v=%d name=%q root=%q}, want {1 my-mesh .}", m.Version, m.Name, m.Root)
	}
	if m.Discovery.GoWork != ModeOn {
		t.Errorf("go_work = %q, want on", m.Discovery.GoWork)
	}
	if m.Discovery.Cargo != ModeAuto {
		t.Errorf("cargo = %q, want auto (defaulted)", m.Discovery.Cargo)
	}
	if m.Defaults.Strategy != "blue-green" || m.Defaults.Replicas != 2 {
		t.Errorf("defaults = %+v", m.Defaults)
	}
	if h := m.Defaults.Health; h == nil || h.Mode != "http" || h.Path != "/healthz" {
		t.Errorf("defaults.health = %+v", m.Defaults.Health)
	}
	if len(m.Services) != 3 {
		t.Fatalf("services = %d, want 3", len(m.Services))
	}
	auth := m.Services["auth"]
	if auth.Port != 8081 || auth.Path != "services/auth" || auth.Strategy != "blue-green" {
		t.Errorf("auth = %+v (path should be slash-cleaned)", auth)
	}
	if auth.Build == nil || len(auth.Build.Args) != 1 || auth.Build.Env["FOO"] != "bar" {
		t.Errorf("auth.build = %+v", auth.Build)
	}
	if got := m.Services["gateway"].DependsOn; len(got) != 2 || got[0] != "auth" || got[1] != "orders" {
		t.Errorf("gateway.depends_on = %v, want [auth orders]", got)
	}
}

func TestParseManifestNormalizesPaths(t *testing.T) {
	src := "version: 1\nroot: ./repo/\nservices:\n  a:\n    path: ./services\\a/\n    port: 1\n"
	m, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Root != "repo" {
		t.Errorf("root = %q, want repo", m.Root)
	}
	if got := m.Services["a"].Path; got != "services/a" {
		t.Errorf("path = %q, want services/a", got)
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFileName)
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if _, err := LoadManifest(filepath.Join(dir, "nope.yaml")); phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		t.Errorf("missing file code = %q, want NOT_FOUND", phelixerr.CodeOf(err))
	}
}
