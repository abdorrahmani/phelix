//go:build !linux

package resources

import (
	"fmt"
	"os/exec"
	"runtime"
)

func Start(cmd *exec.Cmd, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !cfg.IsZero() {
		return fmt.Errorf("resource limits require Linux cgroups v2; unsupported platform %s", runtime.GOOS)
	}
	return cmd.Start()
}

func RunCleanupHelper() bool { return false }
