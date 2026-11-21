package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestContainerInstallerConfigurationAndSafeFailures(t *testing.T) {
	for _, scenario := range []string{"install", "state", "configuration", "docker", "compose", "pull", "active", "enabled", "port", "traversal", "repeat"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(root, "docker.log")
			for name, script := range map[string]string{
				"docker": `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PHELIX_INSTALL_TEST_LOG"
case "$1" in
  info) [[ "$PHELIX_INSTALL_TEST_SCENARIO" != docker ]] ;;
  pull) [[ "$PHELIX_INSTALL_TEST_SCENARIO" != pull ]] ;;
  run) printf '1.2.3\n' ;;
  compose)
    if [[ "$2" == version ]]; then [[ "$PHELIX_INSTALL_TEST_SCENARIO" != compose ]]; fi
    ;;
esac
`,
				"systemctl": "#!/usr/bin/env bash\n[[ \"$PHELIX_INSTALL_TEST_SCENARIO\" == active || ( \"$PHELIX_INSTALL_TEST_SCENARIO\" == enabled && \"$1\" == is-enabled ) ]]\n",
				"sudo":      "#!/usr/bin/env bash\nif [[ \"$1\" == -v ]]; then exit 0; fi\nexec \"$@\"\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			configDir, dataDir := filepath.Join(root, "config"), filepath.Join(root, "state")
			if scenario == "state" || scenario == "configuration" {
				dir := dataDir
				name := "agent-id"
				if scenario == "configuration" {
					dir, name = configDir, "controller.yaml"
				}
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			port := "8080"
			if scenario == "port" {
				port = "65536"
			}
			projectDir := filepath.Join(root, "projects")
			if scenario == "traversal" {
				projectDir += "/../escape"
			}
			args := []string{"install.sh", "--docker", "--version", "1.2.3", "--config-dir", configDir, "--data-dir", dataDir, "--socket-dir", filepath.Join(root, "run"), "--projects-dir", projectDir, "--network", "test-network", "--proxy-port", port}
			command := exec.Command("bash", args...)
			command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PHELIX_INSTALL_TEST_LOG="+log, "PHELIX_INSTALL_TEST_SCENARIO="+scenario)
			output, err := command.CombinedOutput()
			wantFailure := scenario != "install" && scenario != "repeat"
			if (err != nil) != wantFailure {
				t.Fatalf("%v: %s", err, output)
			}
			if wantFailure {
				if scenario == "state" || scenario == "configuration" {
					path := filepath.Join(dataDir, "agent-id")
					if scenario == "configuration" {
						path = filepath.Join(configDir, "controller.yaml")
					}
					data, err := os.ReadFile(path)
					if err != nil || string(data) != "preserve" {
						t.Fatal("existing data changed", err)
					}
				}
				return
			}
			raw, err := os.ReadFile(filepath.Join(configDir, "compose.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var compose struct {
				Services map[string]struct {
					Image      string   `yaml:"image"`
					Ports      []string `yaml:"ports"`
					Privileged bool     `yaml:"privileged"`
					Volumes    []struct {
						Source, Target string
						ReadOnly       bool `yaml:"read_only"`
					} `yaml:"volumes"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal(raw, &compose); err != nil {
				t.Fatal(err)
			}
			if dockerPath, err := exec.LookPath("docker"); err == nil && scenario == "install" {
				if err := exec.Command(dockerPath, "compose", "version").Run(); err == nil {
					if output, err := exec.Command(dockerPath, "compose", "-f", filepath.Join(configDir, "compose.yaml"), "config", "--quiet").CombinedOutput(); err != nil {
						t.Fatalf("Docker Compose rejected generated configuration: %v: %s", err, output)
					}
				}
			}
			service := compose.Services["phelix"]
			if service.Image != "ghcr.io/abdorrahmani/phelix:1.2.3" || service.Privileged || fmt.Sprint(service.Ports) != "[127.0.0.1:8080:8080]" {
				t.Fatal(service)
			}
			mounted := false
			for _, volume := range service.Volumes {
				if volume.Target == "/projects" {
					mounted = volume.ReadOnly && volume.Source == projectDir
				}
			}
			if !mounted {
				t.Fatal("read-only project mount is missing")
			}
			if scenario == "repeat" {
				repeat := exec.Command("bash", args...)
				repeat.Env = command.Env
				output, err = repeat.CombinedOutput()
				if err == nil || !strings.Contains(string(output), "already exists") {
					t.Fatalf("repeat install: %v: %s", err, output)
				}
				current, err := os.ReadFile(filepath.Join(configDir, "compose.yaml"))
				if err != nil || string(current) != string(raw) {
					t.Fatal("repeat changed configuration", err)
				}
			}
		})
	}
}
