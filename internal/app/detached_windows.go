//go:build windows

package app

import "syscall"

// detachedSysProcAttr returns a *syscall.SysProcAttr that detaches the child
// into its own process group on Windows.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200 /* CREATE_NEW_PROCESS_GROUP */}
}
