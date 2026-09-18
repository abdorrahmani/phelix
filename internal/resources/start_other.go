//go:build !linux

package resources

import (
	"fmt"
	"os/exec"
	"runtime"
)

func Start(cmd *exec.Cmd, cfg Config) (*Instance, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.IsZero() {
		return nil, fmt.Errorf("resource limits require Linux cgroups v2; unsupported platform %s", runtime.GOOS)
	}
	return nil, cmd.Start()
}

// Instance is the Linux-only tracking handle for a limited instance (see
// start_linux.go). Other platforms never configure resource limits, so Start
// never returns one; these stubs exist only to keep the runtime layers
// compiling.
type Instance struct{}

func (i *Instance) ResourceOOM() (bool, error) {
	return false, fmt.Errorf("resource limits: unsupported platform %s", runtime.GOOS)
}

func (i *Instance) Snapshot() (Usage, error) {
	return Usage{}, fmt.Errorf("resource limits: unsupported platform %s", runtime.GOOS)
}

func (i *Instance) MemoryLimit() string { return "" }

func (i *Instance) Close() error { return nil }

func RunCleanupHelper() bool { return false }
