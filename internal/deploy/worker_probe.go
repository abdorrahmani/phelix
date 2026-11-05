package deploy

import (
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"os"
	"os/exec"
	"time"
)

// ProbeOutcome is one exec health probe result.
type ProbeOutcome struct {
	OK     bool
	Detail string
}

// RunProbe executes one exec health probe: direct argv (no shell), the app
// directory as working dir, the replica's env overlay, a strict timeout with
// process-group kill, and a bounded output tail for failure details.
func RunProbe(command []string, dir string, env []string, timeout time.Duration) ProbeOutcome {
	if len(command) == 0 {
		return ProbeOutcome{OK: false, Detail: "no probe command configured"}
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out := &boundedBuffer{max: 4096}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := startProbeCommand(cmd); err != nil {
		return ProbeOutcome{OK: false, Detail: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			return ProbeOutcome{OK: true}
		}
		return ProbeOutcome{OK: false, Detail: probeFailureDetail(err, out.String())}
	case <-timer.C:
		killProbe(cmd)
		<-done
		return ProbeOutcome{OK: false, Detail: "timeout after " + timeout.String() + ": " + out.String()}
	}
}

// boundedBuffer is an io.Writer keeping only the last max bytes, so a chatty
// probe cannot grow memory without bound.
type boundedBuffer struct {
	buf      []byte
	max      int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.overflow || len(b.buf)+len(p) > b.max {
		b.overflow = true
		b.buf = nil
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	if b.overflow {
		return "probe output exceeded limit; output withheld"
	}
	return phelixerr.Redact(string(b.buf))
}
