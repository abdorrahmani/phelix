package update

import (
	"os"
	"testing"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

func TestSystemdRequestRestart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root path is covered by the non-root branch in CI")
	}
	runner := &fakeRunner{responses: map[string][]fakeResp{
		"systemctl --version":                                 {{out: "systemd 255"}},
		"systemctl cat phelix.service":                        {{out: "unit"}},
		"sudo -n systemctl restart --no-block phelix.service": {{}},
	}}
	if err := (Systemd{Runner: runner}).RequestRestart(); err != nil {
		t.Fatalf("RequestRestart() error: %v", err)
	}
	if !runner.hasCall("sudo -n systemctl restart --no-block phelix.service") {
		t.Fatalf("unexpected calls: %v", runner.calls)
	}
}

func TestSystemdRequestRestartUnsupported(t *testing.T) {
	runner := &fakeRunner{}
	err := (Systemd{Runner: runner}).RequestRestart()
	if !phelixerr.IsCode(err, phelixerr.CodeUnimplemented) {
		t.Fatalf("error = %v, want CodeUnimplemented", err)
	}

	runner = &fakeRunner{responses: map[string][]fakeResp{
		"systemctl --version": {{out: "systemd 255"}},
	}}
	err = (Systemd{Runner: runner}).RequestRestart()
	if !phelixerr.IsCode(err, phelixerr.CodeUnimplemented) {
		t.Fatalf("missing unit error = %v, want CodeUnimplemented", err)
	}
}
