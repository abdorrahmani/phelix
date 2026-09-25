package resources

import (
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCPU(t *testing.T) {
	for input, want := range map[string]uint64{"500m": 50000, "1000m": 100000, "1": 100000, "2": 200000, "250m": 25000, "0.5": 50000, "10m": 1000} {
		got, err := ParseCPU(input)
		if err != nil || got != want {
			t.Errorf("%s: got %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "-1", "-500m", "abc", "1cpu", "1m", "9m", "1.0001", "01", "+1", " 1", "1e3", "18446744073709551616m", "92233720368548"} {
		if _, err := ParseCPU(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}
func TestMemory(t *testing.T) {
	for input, want := range map[string]uint64{"128Ki": 131072, "512Mi": 536870912, "1Gi": 1073741824, "2Gi": 2147483648, "1Ti": 1099511627776} {
		got, err := ParseMemory(input)
		if err != nil || got != want {
			t.Errorf("%s: got %d,%v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "0", "-1", "512", "512MB", "abc", "0Mi", "1.5Gi", "01Mi", "18446744073709551616Ti", "8388608Ti"} {
		if _, err := ParseMemory(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}
func TestConfigYAML(t *testing.T) {
	for _, input := range []string{"name: api", "resources: {}", "resources:\n  cpu: '500m'", "resources:\n  memory: '512Mi'", "resources:\n  cpu: '2'\n  memory: '1Gi'"} {
		var cfg struct {
			Resources Config `yaml:"resources"`
		}
		if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
			t.Errorf("%s: %v", input, err)
		}
	}
	for _, input := range []string{"resources:\n  cpu: ''", "resources:\n  memory:", "resources:\n  cpu: null", "resources:\n  cpu: 0", "resources:\n  memory: 512MB", "resources:\n  cpu: '1'\n  cpu: '2'", "resources:\n  cpus: '1'", "resources: []"} {
		var cfg struct {
			Resources Config `yaml:"resources"`
		}
		if err := yaml.Unmarshal([]byte(input), &cfg); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatal(err)
	}
}
