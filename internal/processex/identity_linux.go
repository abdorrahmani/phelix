//go:build linux

package processex

import (
	"fmt"
	"os"
)

// Identity reads the full identity of a live process: start time from
// /proc/<pid>/stat (kernel clock ticks) and the executable path from the
// /proc/<pid>/exe symlink. It fails when the process does not exist or the
// stat line cannot be parsed.
func Identity(pid int) (ProcessIdentity, error) {
	if pid <= 0 {
		return ProcessIdentity{}, ErrNoStat
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcessIdentity{}, ErrNoStat
	}
	ticks, err := parseStatStarttime(stat)
	if err != nil {
		return ProcessIdentity{}, err
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("processex: read exe link: %w", err)
	}
	return ProcessIdentity{PID: pid, StartTime: ticks, ExePath: exe}, nil
}

// Matches reports whether a live process currently carries the identity
// captured at spawn: same PID, same kernel start time, same executable. Any
// mismatch or an unreadable identity is a non-match; a recycled PID never
// passes because its start time differs.
func Matches(id ProcessIdentity) bool {
	if id.PID <= 0 {
		return false
	}
	cur, err := Identity(id.PID)
	if err != nil {
		return false
	}
	if id.StartTime != 0 && cur.StartTime != id.StartTime {
		return false
	}
	return sameExe(cur.ExePath, id.ExePath)
}
