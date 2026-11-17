package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func ValidateDockerEnvironment(ctx context.Context, cfg Config) error {
	return validateDockerEnvironment(ctx, cfg, func(ctx context.Context, id string) ([]byte, error) {
		return exec.CommandContext(ctx, "docker", "inspect", id).Output()
	})
}

func validateDockerEnvironment(ctx context.Context, cfg Config, inspect func(context.Context, string) ([]byte, error)) error {
	if cfg.Mode != "container" {
		return nil
	}
	id, err := os.Hostname()
	if err != nil {
		return err
	}
	output, err := inspect(ctx, id)
	if err != nil {
		return fmt.Errorf("cannot inspect Controller container through Docker socket; mount the local Docker socket and retain the default container hostname")
	}
	var objects []struct {
		NetworkSettings struct{ Networks map[string]json.RawMessage }
		Mounts          []struct {
			Destination string
			RW          bool
			Type        string
		}
	}
	if json.Unmarshal(output, &objects) != nil || len(objects) != 1 {
		return fmt.Errorf("invalid Controller Docker metadata")
	}
	if _, ok := objects[0].NetworkSettings.Networks[cfg.Network]; !ok {
		return fmt.Errorf("Controller is not attached to shared network %q", cfg.Network)
	}
	persisted := func(path string, writable bool) bool {
		for _, mount := range objects[0].Mounts {
			relative, err := filepath.Rel(mount.Destination, path)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, "../") && (!writable || mount.RW) && (mount.Type == "bind" || mount.Type == "volume") {
				return true
			}
		}
		return false
	}
	directory := os.Getenv("PHELIX_DATA_DIR")
	if !persisted(directory, true) {
		return fmt.Errorf("Controller data directory %s requires a writable persistent volume or bind mount", directory)
	}
	if !persisted(filepath.Dir(cfg.Socket), true) {
		return fmt.Errorf("Controller socket directory requires a writable shared bind mount or volume")
	}
	for _, source := range cfg.Sources {
		if !persisted(source.Controller, false) {
			return fmt.Errorf("source root %s must be mounted into Controller", source.Controller)
		}
	}
	return nil
}
