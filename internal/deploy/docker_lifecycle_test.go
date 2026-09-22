package deploy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// dockerishProc is a Process that carries a ContainerID, so StartDeployment's
// containerIDOf(proc) records it — the fake stand-in for a launched container.
// PID is 0, mirroring how docker instances are recorded.
type dockerishProc struct {
	selfProc
	cid string
}

func (d *dockerishProc) ContainerID() string { return d.cid }

// dockerHTTPLauncher is a fake docker InstanceLauncher: it stands up a real
// listener (so port.WaitForListener succeeds) and returns a ContainerID-bearing
// Process. It never touches BinaryPath, exactly like the real docker launcher
// treats the image ref.
type dockerHTTPLauncher struct {
	servers []*httptest.Server
	cid     string
	called  bool
}

func (dl *dockerHTTPLauncher) Launch(_ context.Context, _ string, _ []string) (Process, int, error) {
	dl.called = true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	dl.servers = append(dl.servers, srv)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	return &dockerishProc{cid: dl.cid}, port, nil
}

func (dl *dockerHTTPLauncher) close() {
	for _, s := range dl.servers {
		s.Close()
	}
}

// scriptedDocker is a dockerRunner that answers per-subcommand and records
// every call, so a lifecycle test can drive the container stop path (inspect →
// kill → wait) without a daemon.
type scriptedDocker struct {
	running string // "true"/"false" for the .State.Running|managed inspect
	calls   [][]string
}

func (s *scriptedDocker) runner() dockerRunner {
	return func(_ context.Context, args ...string) (string, error) {
		s.calls = append(s.calls, args)
		switch args[0] {
		case "inspect":
			return s.running + "|true\n", nil
		case "wait":
			return "0\n", nil // container exits cleanly on SIGTERM
		default: // kill, stop, ...
			return "", nil
		}
	}
}

func (s *scriptedDocker) sawSubcommand(sub string) bool {
	for _, c := range s.calls {
		if len(c) > 0 && c[0] == sub {
			return true
		}
	}
	return false
}

// TestTeardownDeployment_StopsDockerContainer is the core regression: a docker
// instance records PID 0, so the old PID<=0 skip left the container running
// while teardown wrote "stopped". Teardown must now actually issue a container
// stop (via stopInstance → docker) and mark the record stopped.
func TestTeardownDeployment_StopsDockerContainer(t *testing.T) {
	resetHome(t)
	sd := &scriptedDocker{running: "true"} // alive + managed
	withIdentityRunner(t, sd.runner())

	inst := &Instance{Slot: SlotGreen, Status: "running", PID: 0, ContainerID: "c0ffee", BinaryPath: "billing:v3"}
	state := &DeployState{
		AppName: "billing", Mode: ModeBlueGreen, ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotBlue:  {Slot: SlotBlue, Status: "stopped"},
			SlotGreen: inst,
		},
	}

	if err := TeardownDeployment(context.Background(), state, 500*time.Millisecond, nil); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	// It must have actually asked Docker to stop the container (SIGTERM via
	// `docker kill --signal=...`), not just rewritten the record.
	if !sd.sawSubcommand("kill") {
		t.Fatalf("teardown must issue a container stop for a docker instance, calls: %v", sd.calls)
	}
	if inst.Status != "stopped" || inst.PID != 0 {
		t.Fatalf("docker instance must be recorded stopped, got status=%q pid=%d", inst.Status, inst.PID)
	}
}

// TestDecideLifecycle_DockerInstanceRunning: a live docker instance (PID 0 +
// ContainerID) must decide "running". The old instancesServing PID>0 gate
// excluded it, reporting a live container as stopped (the list/status drift).
func TestDecideLifecycle_DockerInstanceRunning(t *testing.T) {
	resetHome(t)
	sd := &scriptedDocker{running: "true"}
	withIdentityRunner(t, sd.runner())

	state := &DeployState{
		AppName: "billing", Mode: ModeBlueGreen, PublicPort: 3000, ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotGreen: {Slot: SlotGreen, Status: "running", PID: 0, ContainerID: "cafe", BinaryPath: "billing:v3", Port: 32775},
		},
	}
	d := DecideLifecycle(state)
	if d.Status != "running" {
		t.Fatalf("live docker instance must decide running, got %q", d.Status)
	}
	if d.Desired != 1 || d.Alive != 1 {
		t.Fatalf("live docker instance must count as serving, got desired=%d alive=%d", d.Desired, d.Alive)
	}
}

// TestDecideLifecycle_DockerInstanceStopped: once the container is gone
// (State.Running=false), the same record must decide "stopped".
func TestDecideLifecycle_DockerInstanceStopped(t *testing.T) {
	resetHome(t)
	sd := &scriptedDocker{running: "false"}
	withIdentityRunner(t, sd.runner())

	state := &DeployState{
		AppName: "billing", Mode: ModeBlueGreen, PublicPort: 3000, ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotGreen: {Slot: SlotGreen, Status: "running", PID: 0, ContainerID: "cafe", BinaryPath: "billing:v3"},
		},
	}
	if d := DecideLifecycle(state); d.Status != "stopped" {
		t.Fatalf("stopped container must decide stopped, got %q", d.Status)
	}
}

// TestReapStaleInstances_DockerDeadCleared: a docker record whose container is
// gone must be cleared (previously skipped on PID<=0 so it lingered).
func TestReapStaleInstances_DockerDeadCleared(t *testing.T) {
	sd := &scriptedDocker{running: "false"}
	withIdentityRunner(t, sd.runner())

	inst := &Instance{Slot: SlotGreen, Status: "running", PID: 0, ContainerID: "gone", BinaryPath: "billing:v3"}
	state := &DeployState{
		AppName: "billing", Mode: ModeBlueGreen, ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{SlotGreen: inst},
	}
	ReapStaleInstances(state)
	if inst.Status != "stopped" {
		t.Fatalf("dead docker container must be reaped, got status=%q", inst.Status)
	}
}

// TestStartDeployment_DockerSkipsStatAndRecordsContainerID is the core
// regression: a docker instance's BinaryPath is an image ref (not a file), so
// the old unconditional os.Stat aborted with "recorded binary is gone".
// StartDeployment must skip the on-disk stat for a docker runtime, launch via
// the launcher, and record the FRESH container id on the restored instance.
func TestStartDeployment_DockerSkipsStatAndRecordsContainerID(t *testing.T) {
	resetHome(t)
	dl := &dockerHTTPLauncher{cid: "newcafe"}
	defer dl.close()

	state := &DeployState{
		AppName: "billing", AppID: "42", Mode: ModeBlueGreen, PublicPort: 3000,
		Runtime: RuntimeDocker, ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotBlue: {Slot: SlotBlue, Status: "stopped"},
			// image ref, NOT a file; old dead container id from the prior run.
			SlotGreen: {Slot: SlotGreen, Status: "stopped", BinaryPath: "billing:v11", ContainerID: "olddead"},
		},
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}

	err := StartDeployment(context.Background(), state,
		StartOptions{Launcher: dl.Launch, ListenTimeout: 2 * time.Second, Logger: &fakeLogger{}})
	if err != nil {
		t.Fatalf("docker start must not fail on the image-ref stat, got: %v", err)
	}
	if !dl.called {
		t.Fatal("the docker launcher must be invoked")
	}
	inst := mustLoad(t, "billing").Slots[SlotGreen]
	if inst.Status != "running" {
		t.Fatalf("restored docker instance must be running, got %q", inst.Status)
	}
	if inst.ContainerID != "newcafe" {
		t.Fatalf("must record the fresh container id, got %q", inst.ContainerID)
	}
}

// TestStartDeployment_EmptyRuntimeWithContainerIDTreatedAsDocker: an older state
// that predates the Runtime field but whose instance carries a ContainerID must
// be restored as docker (stat skipped), not regressed into the native launcher.
func TestStartDeployment_EmptyRuntimeWithContainerIDTreatedAsDocker(t *testing.T) {
	resetHome(t)
	dl := &dockerHTTPLauncher{cid: "c2"}
	defer dl.close()

	state := &DeployState{
		AppName: "legacy", AppID: "7", Mode: ModeBlueGreen, PublicPort: 3000,
		Runtime: "", ActiveSlot: SlotGreen, // no persisted runtime
		Slots: map[string]*Instance{
			SlotGreen: {Slot: SlotGreen, Status: "stopped", BinaryPath: "legacy:v3", ContainerID: "prev"},
		},
	}
	if err := Store(state); err != nil {
		t.Fatal(err)
	}
	if err := StartDeployment(context.Background(), state,
		StartOptions{Launcher: dl.Launch, ListenTimeout: 2 * time.Second, Logger: &fakeLogger{}}); err != nil {
		t.Fatalf("empty-runtime docker instance must not hit the stat guard, got: %v", err)
	}
}

// TestStartDeployment_NativeMissingBinaryStillErrors guards that the native path
// is unchanged: a native state (no runtime, no ContainerID) with a bogus binary
// still fails NOT_FOUND on the on-disk stat.
func TestStartDeployment_NativeMissingBinaryStillErrors(t *testing.T) {
	resetHome(t)
	state := &DeployState{
		AppName: "nat", Mode: ModeBlueGreen, ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotGreen: {Slot: SlotGreen, Status: "stopped", BinaryPath: "/nonexistent/app_bin"},
		},
	}
	err := StartDeployment(context.Background(), state, StartOptions{})
	if err == nil {
		t.Fatal("native missing binary must still fail")
	}
	if !phelixerr.IsCode(err, phelixerr.CodeNotFound) {
		t.Fatalf("native missing binary must be NOT_FOUND, got %s", phelixerr.CodeOf(err))
	}
}

// TestDecideLifecycle_NativeStoppedUnaffected guards that the Container|PID
// widening did not make a cleanly-stopped native instance (PID 0, no
// ContainerID) look like it is serving.
func TestDecideLifecycle_NativeStoppedUnaffected(t *testing.T) {
	resetHome(t)
	state := &DeployState{
		AppName: "native", Mode: ModeBlueGreen, ActiveSlot: SlotGreen,
		Slots: map[string]*Instance{
			SlotGreen: {Slot: SlotGreen, Status: "stopped", PID: 0}, // no ContainerID
		},
	}
	if d := DecideLifecycle(state); d.Status != "stopped" || d.Desired != 0 {
		t.Fatalf("stopped native instance must stay stopped/not-serving, got status=%q desired=%d", d.Status, d.Desired)
	}
}
