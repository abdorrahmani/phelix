package deploy

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/health"
)

// oomLauncher returns a dead-on-arrival port (health fails) whose process
// handle reports RESOURCE_OOM for the exit — what launchInstance produces for
// a candidate killed by its cgroup memory limit before becoming healthy.
type oomLauncher struct{}

func (oomLauncher) Launch(_ context.Context, _ string, _ []string) (Process, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return &oomProc{pid: os.Getpid() + 100000}, port, nil
}

func TestBlueGreenCandidateOOMFailsDeployKeepsActiveServing(t *testing.T) {
	resetHome(t)

	pc := &fakeProxyClient{alive: true}
	fl := &httpLauncher{}
	defer fl.close()

	bg := &BlueGreen{
		AppName: "appOom", AppID: "9", PublicPort: 0,
		Builder:     stubBuilder("/bin/true"),
		Launcher:    fl.Launch,
		ProxyClient: pc,
		Logger:      &fakeLogger{},
		HealthProvider: func(string) *health.DeployTierConfig {
			return fastHealthShortTimeout()
		},
		GracePeriod: 10 * time.Millisecond,
	}
	// First deploy succeeds; that instance stays serving for the whole test.
	if err := bg.Deploy(context.Background()); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	before := mustLoad(t, "appOom")
	firstActive := before.ActiveSlot
	firstPID := before.Slots[firstActive].PID
	pc.mu.Lock()
	switchesBefore := len(pc.switches)
	pc.mu.Unlock()

	// Second deploy: the candidate exceeds its memory limit and dies.
	bg.Launcher = oomLauncher{}.Launch
	err := bg.Deploy(context.Background())
	if !phelixerr.IsCode(err, phelixerr.CodeResourceOOM) {
		t.Fatalf("want RESOURCE_OOM, got %v / %s", err, phelixerr.CodeOf(err))
	}

	// The OOM candidate never became the serving instance.
	after := mustLoad(t, "appOom")
	if after.ActiveSlot != firstActive || after.Slots[firstActive].PID != firstPID {
		t.Fatalf("active instance changed: slot %q pid %d -> %q pid %d",
			firstActive, firstPID, after.ActiveSlot, after.Slots[firstActive].PID)
	}
	pc.mu.Lock()
	switchesAfter := len(pc.switches)
	pc.mu.Unlock()
	if switchesAfter != switchesBefore {
		t.Fatalf("traffic switched to the OOM candidate: %d switches (before %d)", switchesAfter, switchesBefore)
	}
	other := after.InactiveSlot()
	if after.Slots[other].Status != "failed" || after.Slots[other].PID != 0 {
		t.Fatalf("failed candidate not recorded as failed: %+v", after.Slots[other])
	}
}

func TestRollingReplacementOOMUsesExistingFailurePath(t *testing.T) {
	resetHome(t)
	app := "rolling-oom"
	old := &Instance{Slot: "0", PID: os.Getpid(), Port: 43210, BinaryPath: "/bin/true", EnvPath: "/old/env", Status: "running", Version: 7, StartedAt: time.Unix(12, 0)}
	state := &DeployState{AppName: app, Mode: ModeRolling, PublicPort: 8080, Replicas: map[string]*Instance{"0": cloneInstance(old)}}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	r := &Rolling{AppName: app, PublicPort: 8080, Replicas: 1, Builder: stubBuilder("/bin/true"), Launcher: oomLauncher{}.Launch, ProxyClient: &fakeProxyClient{alive: true}, Logger: &fakeLogger{}, HealthProvider: func(string) *health.DeployTierConfig { return fastHealthShortTimeout() }, GracePeriod: 10 * time.Millisecond}

	err := r.Deploy(context.Background())
	if !phelixerr.IsCode(err, phelixerr.CodeResourceOOM) {
		t.Fatalf("want RESOURCE_OOM, got %v / %s", err, phelixerr.CodeOf(err))
	}

	// Existing recovery semantics: the OOM replacement was rejected and the
	// complete old record keeps serving.
	got := mustLoad(t, app).Replicas["0"]
	if *got != *old {
		t.Fatalf("old record not fully restored after OOM replacement:\n got %+v\nwant %+v", got, old)
	}
}
