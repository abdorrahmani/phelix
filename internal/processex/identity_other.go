//go:build !linux

package processex

import (
	"errors"

	"github.com/shirou/gopsutil/process"
)

// ErrUnavailable reports a platform without a start-time identity source.
var ErrUnavailable = errors.New("processex: start-time identity unavailable on this platform")

// Identity reads the identity of a live process on platforms without
// /proc: only the PID is available, so exe and start time stay empty and
// matching degrades to liveness (the classic app manager's strength).
func Identity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, ErrUnavailable
	}
	return ProcessIdentity{PID: pid}, nil
}

// Matches reports whether a process with the given PID is alive. Without a
// start-time source the identity check cannot exclude a recycled PID.
func Matches(id ProcessIdentity) bool {
	if id.PID <= 0 {
		return false
	}
	ok, err := process.PidExists(int32(id.PID))
	return err == nil && ok
}
