package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

type configFixture struct {
	Name   string
	YAML   string
	Config *Config
	Error  string
}

func TestConfigReference(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []configFixture
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	original := embeddedConfig
	t.Cleanup(func() { embeddedConfig = original; cfg = nil; once = sync.Once{} })
	for _, key := range []string{"PHELIX_MODE", "PHELIX_API", "PHELIX_GRPC_URL"} {
		t.Setenv(key, "")
	}
	for i := range fixtures {
		f := &fixtures[i]
		t.Run(f.Name, func(t *testing.T) {
			embeddedConfig = []byte(f.YAML)
			cfg = nil
			once = sync.Once{}
			if Get() != nil {
				t.Fatal("Get before Load must be nil")
			}
			err := Load()
			message := ""
			if err != nil {
				if !phelixerr.IsCode(err, phelixerr.CodeConfiguration) {
					t.Fatalf("error code: %v", err)
				}
				message = err.Error()
			}
			if !reflect.DeepEqual(Get(), f.Config) || message != f.Error {
				t.Fatalf("got %+v, %q; want %+v, %q", Get(), message, f.Config, f.Error)
			}
			loaded := Get()
			embeddedConfig = []byte("invalid: [")
			if err := Load(); err != nil || Get() != loaded {
				t.Fatalf("subsequent Load changed result: %v", err)
			}
		})
	}
}

func TestEnvironmentOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, mode, api, grpc string
		want                  AppConfig
	}{
		{"empty", "", "", "", AppConfig{"production", "https://example.test/api", "example.test:443"}},
		{"whitespace", " \t", " \n", " ", AppConfig{"production", "https://example.test/api", "example.test:443"}},
		{"mode", " DeV ", "", "", AppConfig{"dev", "https://example.test/api", "example.test:443"}},
		{"api", "", " https://EXAMPLE.test/API ", "", AppConfig{"production", "https://EXAMPLE.test/API", "example.test:443"}},
		{"grpc", "", "", " LOCALHOST:1234 ", AppConfig{"production", "https://example.test/api", "LOCALHOST:1234"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PHELIX_MODE", tc.mode)
			t.Setenv("PHELIX_API", tc.api)
			t.Setenv("PHELIX_GRPC_URL", tc.grpc)
			c := Config{App: AppConfig{"production", "https://example.test/api", "example.test:443"}, Alert: AlertConfig{80, 90, 95}}
			applyEnvOverrides(&c)
			if c.App != tc.want || c.Alert != (AlertConfig{80, 90, 95}) {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestLoadAppliesEnvironmentOnce(t *testing.T) {
	original := embeddedConfig
	t.Cleanup(func() { embeddedConfig = original; cfg = nil; once = sync.Once{} })
	embeddedConfig = []byte("app:\n  mode: production\n  api: https://example.test\n  grpcUrl: example.test:443\n")
	cfg = nil
	once = sync.Once{}
	t.Setenv("PHELIX_MODE", " DEV ")
	t.Setenv("PHELIX_API", " https://override.test ")
	t.Setenv("PHELIX_GRPC_URL", " override.test:443 ")
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	if Get().App != (AppConfig{"dev", "https://override.test", "override.test:443"}) {
		t.Fatalf("got %+v", Get())
	}
	t.Setenv("PHELIX_MODE", "production")
	if err := Load(); err != nil || Get().App.Mode != "dev" {
		t.Fatalf("Load reapplied overrides: %v", err)
	}
}

func BenchmarkConfigLoad(b *testing.B) {
	for _, key := range []string{"PHELIX_MODE", "PHELIX_API", "PHELIX_GRPC_URL"} {
		b.Setenv(key, "")
	}
	b.Cleanup(func() { cfg = nil; once = sync.Once{} })
	b.ReportAllocs()
	for b.Loop() {
		cfg = nil
		once = sync.Once{}
		if err := Load(); err != nil {
			b.Fatal(err)
		}
		if !strings.Contains(Get().App.API, "://") {
			b.Fatal("API missing")
		}
	}
}
