package deploy

import (
	"errors"
	"os"
	"strings"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// oomProc simulates an instance that died of its cgroup memory limit: its
// exit (Wait) reports RESOURCE_OOM, mirroring what launchInstance's
// classification produces for a real OOM-killed instance.
type oomProc struct{ pid int }

func (p *oomProc) PID() int               { return p.pid }
func (p *oomProc) Signal(os.Signal) error { return os.ErrProcessDone }
func (p *oomProc) Kill() error            { return os.ErrProcessDone }
func (p *oomProc) Wait() error {
	return oomExitError(errors.New("signal: killed"), p.pid, "16Mi")
}

func TestOOMExitError(t *testing.T) {
	cause := errors.New("signal: killed")
	err := oomExitError(cause, 42, "16Mi")
	if !phelixerr.IsCode(err, phelixerr.CodeResourceOOM) {
		t.Fatalf("want RESOURCE_OOM, got %s", phelixerr.CodeOf(err))
	}
	if !errors.Is(err, cause) {
		t.Fatalf("original wait error lost: %v", err)
	}

	// A descendant OOM with the main process exiting zero still classifies.
	err = oomExitError(nil, 7, "512Mi")
	if !phelixerr.IsCode(err, phelixerr.CodeResourceOOM) {
		t.Fatalf("nil wait error not classified: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "512Mi") || !strings.Contains(err.Error(), "pid 7") {
		t.Fatalf("error lost pid/limit context: %v", err)
	}
}

func TestOOMFailureUpgradesCodeKeepsChain(t *testing.T) {
	failure := phelixerr.Newf(phelixerr.CodeHealthCheckFailed, "probe target: http://127.0.0.1:1")
	upgraded := oomFailure(&oomProc{pid: 9}, failure, "deploy aborted: memory limit exceeded")
	if !phelixerr.IsCode(upgraded, phelixerr.CodeResourceOOM) {
		t.Fatalf("want RESOURCE_OOM, got %s", phelixerr.CodeOf(upgraded))
	}
	if !errors.Is(upgraded, failure) {
		t.Fatal("original health-check failure lost from chain")
	}

	// A process without resource-OOM evidence keeps the original failure.
	same := oomFailure(&selfProc{pid: 1}, failure, "deploy aborted: memory limit exceeded")
	if same != failure {
		t.Fatalf("non-oom exit was re-tagged: %v", same)
	}
}
