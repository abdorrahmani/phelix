package errreport

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/port"
)

// FindListener identifies the process occupying a port. It defaults to
// port.FindListener (read-only, best-effort) and is a variable only so tests
// can substitute a deterministic stub instead of probing the host.
var FindListener = port.FindListener

// goos is the OS used to pick platform-appropriate suggestion commands. It is
// a variable only so tests can exercise every platform branch.
var goos = runtime.GOOS

// inUsePortRe extracts the port number from Phelix's own PORT_UNAVAILABLE
// "already in use" message (port.EnsureAvailable). Matching our own message
// shape — on top of the structured code — keeps this resolver narrow: other
// PORT_UNAVAILABLE failures (proxy bind errors, allocation races, the
// "nothing is listening" port-validation failure) are not classified here.
var inUsePortRe = regexp.MustCompile(`(?i)port (\d+) is already in use`)

// portResolver explains PORT_UNAVAILABLE "already in use" errors: which port
// is occupied, who occupies it when the OS can tell us safely, and read-only
// commands to inspect it. It never suggests stopping the process outright and
// never stops anything itself — that choice belongs to the user.
type portResolver struct{}

func (portResolver) Resolve(err error) (Report, bool) {
	if !phelixerr.IsCode(err, phelixerr.CodePortUnavailable) {
		return Report{}, false
	}
	m := inUsePortRe.FindStringSubmatch(chainText(err))
	if m == nil {
		return Report{}, false
	}
	portNum, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return Report{}, false
	}

	rep := Report{
		Code:  phelixerr.CodePortUnavailable,
		Title: fmt.Sprintf("Port %d is already in use", portNum),
		Explanation: fmt.Sprintf(
			"Port %d is already being used by another process, so Phelix cannot listen on it. Only one process can use a TCP port at a time.",
			portNum),
		Suggestion: fmt.Sprintf(
			"Inspect which process holds the port below. If it is no longer needed, stop it yourself — Phelix never stops it for you — or run your command again on a different port (for example --port %d).",
			portNum+1),
		Command: inspectPortCommand(portNum),
	}
	if proc, ok := FindListener(portNum); ok {
		rep.Explanation += fmt.Sprintf("\nCurrently listening: %s (PID: %d).", proc.Name, proc.PID)
	}
	return rep, true
}

// inspectPortCommand returns a read-only command that shows which process is
// listening on the port, per platform. It never signals or kills anything.
func inspectPortCommand(portNum int) string {
	switch goos {
	case "windows":
		return "netstat -ano | findstr :" + strconv.Itoa(portNum)
	case "darwin":
		return "lsof -i :" + strconv.Itoa(portNum)
	default:
		return "sudo lsof -i :" + strconv.Itoa(portNum)
	}
}
