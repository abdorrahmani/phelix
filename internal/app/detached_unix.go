//go:build !windows

package app

import "syscall"

// detachedSysProcAttr returns a *syscall.SysProcAttr that detaches the child
// into its own session so managed applications survive the short-lived CLI
// process that started them (exit, terminal hangup, Ctrl+C).
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
