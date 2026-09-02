package port

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ProcessInfo identifies a process listening on a TCP port. It is
// best-effort diagnostic information: Name and PID come from the operating
// system and may be unavailable (missing tools, permissions, unknown
// platform).
type ProcessInfo struct {
	Name string
	PID  int
}

// runLsof executes the lsof field-mode query for listeners on the given port.
// It is a var so tests can substitute canned output instead of depending on
// the host's installed tools.
var runLsof = func(portNum int) (string, error) {
	// Every argv element is a compile-time constant except the port address,
	// which is rendered from a range-checked int via %d (digits only).
	// exec.Command runs lsof directly — no shell is ever involved.
	arg := fmt.Sprintf(":%d", portNum)
	out, err := exec.Command("lsof", "-nP", "-i", arg, "-sTCP:LISTEN", "-Fpc").Output()
	return string(out), err
}

// FindListener reports which process is listening on the given TCP port.
// It is strictly read-only: it inspects, never signals or kills. It returns
// false when the lookup is impossible (lsof missing, no listener found,
// unsupported platform) — callers must treat the identification as optional
// context, not as a precondition.
func FindListener(portNum int) (ProcessInfo, bool) {
	if portNum <= 0 || portNum > 65535 {
		return ProcessInfo{}, false
	}
	out, err := runLsof(portNum)
	if err != nil {
		return ProcessInfo{}, false
	}
	return parseLsofFields(out)
}

// parseLsofFields parses `lsof -Fpc` field output: lines of the form
// "p<pid>" (process ID) and "c<command>" (command name), possibly repeated
// per file descriptor. The first pid/command pair wins.
func parseLsofFields(out string) (ProcessInfo, bool) {
	var info ProcessInfo
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			if pid, err := strconv.Atoi(line[1:]); err == nil && info.PID == 0 {
				info.PID = pid
			}
		case 'c':
			if info.Name == "" {
				info.Name = line[1:]
			}
		}
	}
	return info, info.PID > 0 && info.Name != ""
}
