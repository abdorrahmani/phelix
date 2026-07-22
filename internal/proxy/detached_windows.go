//go:build windows

package proxy

import "syscall"

// DetachedSysProcAttr returns a *syscall.SysProcAttr that detaches the child
// into its own process group on Windows.
func DetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200 /* CREATE_NEW_PROCESS_GROUP */}
}

func detachedSysProcAttr() *syscall.SysProcAttr { return DetachedSysProcAttr() }
