package update

import (
	"os"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// RequestRestart queues a detached restart of the phelix.service unit. It is
// the only remote-triggered service operation: the argv is fixed, no shell is
// involved, and --no-block lets this process send its accepted result before
// systemd stops it. Non-systemd hosts and hosts without the unit fail closed.
func (s Systemd) RequestRestart() error {
	if !s.Available() {
		return phelixerr.New(phelixerr.CodeUnimplemented,
			"monitor restart is only supported on systemd hosts")
	}
	if !s.UnitExists() {
		return phelixerr.Newf(phelixerr.CodeUnimplemented,
			"monitor restart requires the installed %s.service unit", s.unit())
	}

	var err error
	if os.Geteuid() == 0 {
		_, err = s.run("restart", "--no-block", s.unit()+".service")
	} else {
		_, err = s.Runner.Run("sudo", "-n", "systemctl", "restart", "--no-block", s.unit()+".service")
	}
	if err != nil {
		return phelixerr.Wrapf(phelixerr.CodeUpdateFailed, err,
			"could not queue restart of %s.service", s.unit())
	}
	return nil
}
