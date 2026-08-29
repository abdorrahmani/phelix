package update

import (
	"strings"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/syscmd"
)

// defaultUnit is the systemd unit install.sh creates:
// /etc/systemd/system/phelix.service running `phelix monitor` with
// KillMode=process (stopping the monitor never kills managed apps).
const defaultUnit = "phelix"

// waitActiveInterval is how often WaitActive polls systemctl while waiting
// for the restarted service to become active.
const waitActiveInterval = 500 * time.Millisecond

// Systemd coordinates the phelix monitor systemd service during an update.
// Queries (does the unit exist, is it active) run unprivileged; Restart
// escalates through sudo when the updater is not root.
type Systemd struct {
	// Unit is the unit name without the .service suffix.
	Unit string
	// Runner executes systemctl; injectable for tests.
	Runner syscmd.Runner
	// Poll overrides the WaitActive polling interval (tests).
	Poll time.Duration
}

func (s Systemd) unit() string {
	if s.Unit == "" {
		return defaultUnit
	}
	return s.Unit
}

func (s Systemd) run(args ...string) (string, error) {
	return s.Runner.Run("systemctl", args...)
}

// Available reports whether systemctl exists at all. When it does not (or
// the system is not booted with systemd), the update proceeds as a
// binary-only replacement, mirroring install.sh's --no-service path.
func (s Systemd) Available() bool {
	if s.Runner == nil {
		return false
	}
	_, err := s.Runner.Run("systemctl", "--version")
	return err == nil
}

// UnitExists reports whether the phelix.service unit is installed.
func (s Systemd) UnitExists() bool {
	_, err := s.run("cat", s.unit()+".service")
	return err == nil
}

// IsActive reports whether the unit is currently active. A query that
// cannot be answered (systemd not running, unit missing) is reported as
// inactive, not as an error — the update then never touches the service.
func (s Systemd) IsActive() bool {
	out, err := s.run("is-active", s.unit()+".service")
	if err != nil && strings.TrimSpace(out) != "active" {
		return false
	}
	return strings.TrimSpace(out) == "active"
}

// Restart restarts the unit, wrapping systemctl in sudo when elevate is set
// (non-root updaters cannot restart system units).
func (s Systemd) Restart(elevate bool) error {
	var err error
	if elevate {
		_, err = s.Runner.Run("sudo", "systemctl", "restart", s.unit()+".service")
	} else {
		_, err = s.run("restart", s.unit()+".service")
	}
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeUpdateFailed, err, "could not restart %s.service", s.unit())
	}
	return nil
}

// WaitActive polls until the unit reports active or the timeout elapses.
func (s Systemd) WaitActive(timeout time.Duration) error {
	poll := s.Poll
	if poll <= 0 {
		poll = waitActiveInterval
	}
	deadline := time.Now().Add(timeout)
	for {
		if s.IsActive() {
			return nil
		}
		if !time.Now().Add(poll).Before(deadline) {
			return phelixerr.Newf(phelixerr.CodeTimeout,
				"%s.service did not become active within %s", s.unit(), timeout)
		}
		time.Sleep(poll)
	}
}
