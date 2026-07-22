//go:build !windows

package proxy

import "syscall"

// DetachedSysProcAttr returns a *syscall.SysProcAttr that detaches the child
// into its own session so it survives the parent CLI process exiting.
func DetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// detachedSysProcAttr is kept for EnsureDaemon's internal use.
func detachedSysProcAttr() *syscall.SysProcAttr { return DetachedSysProcAttr() }
