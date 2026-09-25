package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectResources(t *testing.T) {
	for _, tc := range []struct{ yaml, cpu, memory string }{
		{"name: api", "", ""}, {"name: api\nresources: {}", "", ""},
		{"name: api\nresources:\n  cpu: '500m'", "500m", ""},
		{"name: api\nresources:\n  memory: '512Mi'", "", "512Mi"},
		{"name: api\nresources:\n  cpu: '2'\n  memory: '1Gi'", "2", "1Gi"},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(tc.yaml), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Resources.CPU != tc.cpu || cfg.Resources.Memory != tc.memory {
			t.Fatalf("wrong resources: %+v", cfg.Resources)
		}
		if err := Save(dir, cfg); err != nil {
			t.Fatal(err)
		}
		round, err := Load(dir)
		if err != nil || round.Resources != cfg.Resources {
			t.Fatalf("round trip: %+v %v", round, err)
		}
	}
	for _, value := range []string{"cpu: ''", "memory: null", "cpu: -1", "memory: 512MB", "cpu: '18446744073709551616'"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte("name: api\nresources:\n  "+value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(dir); err == nil {
			t.Errorf("accepted %s", value)
		}
	}
}
