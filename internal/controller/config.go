package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/abdorrahmani/phelix/internal/server"
	"go.yaml.in/yaml/v3"
)

type SourceMount struct {
	Host       string `yaml:"host"`
	Controller string `yaml:"controller"`
}

type Config struct {
	SocketUID     *int          `yaml:"socket_uid"`
	SocketGID     *int          `yaml:"socket_gid"`
	Socket        string        `yaml:"socket"`
	AllowedUIDs   []uint32      `yaml:"allowed_uids"`
	Mode          string        `yaml:"mode"`
	Network       string        `yaml:"network"`
	Sources       []SourceMount `yaml:"sources"`
	MaxOperations int           `yaml:"max_operations"`
}

func LoadConfig() (Config, error) {
	path := os.Getenv("PHELIX_CONTROLLER_CONFIG")
	if path == "" {
		path = filepath.Join(server.DataDir(), "controller.yaml")
	}
	return loadConfig(path)
}

func LoadClientConfig() (Config, error) {
	return loadClientConfig("/etc/phelix/client.yaml")
}

func loadClientConfig(installed string) (Config, error) {
	if os.Getenv("PHELIX_CONTROLLER_CONFIG") == "" && os.Getenv("PHELIX_CONTROL_SOCKET") == "" && os.Getenv("PHELIX_DATA_DIR") == "" {
		if _, err := os.Stat(filepath.Join(server.DataDir(), "controller.yaml")); os.IsNotExist(err) {
			return loadConfig(installed)
		}
	}
	return LoadConfig()
}

func loadConfig(path string) (Config, error) {
	c := Config{Socket: filepath.Join(server.DataDir(), "control.sock"), Mode: "host", MaxOperations: 8}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return c, err
	}
	if err == nil {
		d := yaml.NewDecoder(strings.NewReader(string(raw)))
		d.KnownFields(true)
		if err := d.Decode(&c); err != nil {
			return c, fmt.Errorf("controller configuration: %w", err)
		}
	}
	if socket := os.Getenv("PHELIX_CONTROL_SOCKET"); socket != "" {
		c.Socket = socket
	}
	if !filepath.IsAbs(c.Socket) || len(c.Socket) > 100 {
		return c, fmt.Errorf("controller socket must be an absolute path of at most 100 bytes")
	}
	if c.Mode != "host" && c.Mode != "container" {
		return c, fmt.Errorf("controller mode must be host or container")
	}
	if c.Mode == "container" && c.Network == "" {
		return c, fmt.Errorf("container mode requires a shared Docker network")
	}
	if c.SocketUID != nil && *c.SocketUID < 0 {
		return c, fmt.Errorf("socket_uid must be nonnegative")
	}
	if c.SocketGID != nil && *c.SocketGID < 0 {
		return c, fmt.Errorf("socket_gid must be nonnegative")
	}
	if c.MaxOperations < 1 || c.MaxOperations > 128 {
		return c, fmt.Errorf("max_operations must be between 1 and 128")
	}
	for _, mount := range c.Sources {
		if !filepath.IsAbs(mount.Host) || !filepath.IsAbs(mount.Controller) {
			return c, fmt.Errorf("source mounts must use absolute host and controller paths")
		}
	}
	return c, nil
}

func (c Config) SourcePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("source path must be absolute")
	}
	path = filepath.Clean(path)
	mapped := path
	allowed := c.Mode == "host" && len(c.Sources) == 0
	for _, mount := range c.Sources {
		relative, err := filepath.Rel(mount.Host, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			relative, err = filepath.Rel(mount.Controller, path)
		}
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			mapped = filepath.Join(mount.Controller, relative)
			root, err := filepath.EvalSymlinks(mount.Controller)
			if err != nil {
				return "", fmt.Errorf("source mount %q unavailable in Controller: %w", mount.Controller, err)
			}
			resolved, err := filepath.EvalSymlinks(mapped)
			if err != nil {
				return "", fmt.Errorf("source %q unavailable in Controller; mount the project workspace: %w", mapped, err)
			}
			relative, err = filepath.Rel(root, resolved)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("source escapes its configured mount")
			}
			mapped, allowed = resolved, true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("source %q has no configured Controller source mount", path)
	}
	info, err := os.Stat(mapped)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("source %q unavailable in Controller; configure a workspace bind mount", mapped)
	}
	return mapped, nil
}
