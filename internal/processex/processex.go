// Package processex identifies host processes beyond their bare PID so a
// recycled PID is never mistaken for the process Phelix started. On Linux the
// identity combines the PID, the kernel start time from /proc/<pid>/stat
// (field 22, in clock ticks since boot) and the executable path; a match
// requires all of them. Other platforms fall back to the executable path
// alone, the same strength the classic app manager has always had. The
// build-tagged files behind this package are the platform seam: adding a
// platform means adding one identity_<os>.go file.
package processex

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcessIdentity is the spawn-time fingerprint of one process.
type ProcessIdentity struct {
	// PID is the OS process id.
	PID int `json:"pid"`
	// StartTime is the process start time in kernel clock ticks since boot,
	// read from /proc/<pid>/stat field 22 on Linux. Zero on platforms
	// without the field (and for records that predate worker supervision).
	StartTime uint64 `json:"start_time,omitempty"`
	// ExePath is the resolved executable path at identification time.
	ExePath string `json:"exe_path,omitempty"`
}

// sameExe reports whether a running process's resolved executable path refers
// to the same binary as want. It tolerates the kernel's " (deleted)" marker
// for replaced binaries and path cleaning differences.
func sameExe(exe, want string) bool {
	if exe == "" || want == "" {
		return false
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	want = strings.TrimSuffix(want, " (deleted)")
	if filepath.Clean(exe) == filepath.Clean(want) {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		return filepath.Clean(exe) == filepath.Clean(resolved)
	}
	return false
}

// ErrNoStat reports a /proc stat line this package could not parse.
var ErrNoStat = errors.New("processex: unparseable /proc stat")

// starttimeRemainderIndex locates starttime within the tokens that follow
// comm: field 3 (state) is the first token after ')', and starttime is
// overall field 22, so it is remainder token 22-3 = 19 (zero-based).
const starttimeRemainderIndex = 19

// parseStatStarttime extracts field 22 (starttime) from the contents of one
// /proc/<pid>/stat file. comm (field 2) may contain spaces and parentheses,
// so parsing starts after the last ')'.
func parseStatStarttime(stat []byte) (uint64, error) {
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, ErrNoStat
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) <= starttimeRemainderIndex {
		return 0, ErrNoStat
	}
	ticks, err := strconv.ParseUint(fields[starttimeRemainderIndex], 10, 64)
	if err != nil {
		return 0, ErrNoStat
	}
	return ticks, nil
}
