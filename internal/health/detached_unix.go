//go:build !windows

package health

import "syscall"

// DetachedSysProcAttr returns a *syscall.SysProcAttr that detaches the child
// into its own session so it survives the parent CLI process exiting.
func DetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
