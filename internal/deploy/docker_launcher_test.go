package deploy

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// fakeDocker records the docker invocations it receives and returns scripted
// stdout/err per subcommand, so the launcher can be exercised without a Docker
// daemon.
type fakeDocker struct {
	calls   [][]string
	run     string // stdout for `docker run`
	inspect string // stdout for `docker inspect`
	wait    string // stdout for `docker wait`
	logs    string // stdout for `docker logs`
	errs    map[string]error
}

func (f *fakeDocker) runner() dockerRunner {
	return func(_ context.Context, args ...string) (string, error) {
		f.calls = append(f.calls, args)
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		if f.errs != nil {
			if err, ok := f.errs[sub]; ok {
				return "", err
			}
		}
		switch sub {
		case "run":
			return f.run, nil
		case "inspect":
			// port inspect vs pid inspect vs state — return whatever was scripted
			return f.inspect, nil
		case "wait":
			return f.wait, nil
		case "logs":
			return f.logs, nil
		default:
			return "", nil
		}
	}
}

func TestDockerLauncher_StartsContainerAndReportsPort(t *testing.T) {
	fd := &fakeDocker{run: "abc123def456\n", inspect: "49215\n"}
	launcher := newDockerLauncher("billing", "blue", "", fd.runner())

	proc, port, err := launcher(context.Background(), "billing:v3", []string{"DATABASE_URL=postgres://x"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if port != 49215 {
		t.Fatalf("port = %d, want 49215", port)
	}
	dp, ok := proc.(*dockerProcess)
	if !ok {
		t.Fatalf("proc type = %T, want *dockerProcess", proc)
	}
	if dp.ContainerID() != "abc123def456" {
		t.Fatalf("container id = %q", dp.ContainerID())
	}

	// The run call must carry the phelix labels, the loopback publish, an
	// --env-file (never inline -e secrets), and the image reference last.
	run := fd.calls[0]
	joined := strings.Join(run, " ")
	for _, want := range []string{
		DockerLabelManaged + "=true",
		DockerLabelApp + "=billing",
		DockerLabelSlot + "=blue",
		"--env-file",
		"billing:v3",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run args missing %q: %v", want, run)
		}
	}
	if !strings.Contains(joined, "127.0.0.1::") {
		t.Errorf("publish must bind loopback only: %v", run)
	}
	// With no network configured, no --network is passed (default bridge), and a
	// --network-alias is NEVER emitted (see the launcher's blue-green safety note).
	if strings.Contains(joined, "--network") {
		t.Errorf("no network configured, but --network was passed: %v", run)
	}
	if strings.Contains(joined, "--network-alias") {
		t.Errorf("--network-alias must never be set on a managed app container: %v", run)
	}
	// The secret value must never appear on the argv.
	if strings.Contains(joined, "postgres://x") {
		t.Errorf("secret leaked onto docker argv: %v", run)
	}
	if run[len(run)-1] != "billing:v3" {
		t.Errorf("image ref must be the last arg, got %q", run[len(run)-1])
	}
}

func TestDeployState_NetworkRoundTrips(t *testing.T) {
	resetHome(t)
	state := &DeployState{
		AppName: "billing", AppID: "42", Mode: ModeBlueGreen, PublicPort: 8080,
		Runtime: RuntimeDocker, Network: "myproj_appnet",
	}
	if err := Store(state); err != nil {
		t.Fatalf("store: %v", err)
	}
	got := mustLoad(t, "billing")
	if got.Network != "myproj_appnet" {
		t.Fatalf("network round-trip failed: got %q, want myproj_appnet", got.Network)
	}
	if got.Runtime != RuntimeDocker {
		t.Fatalf("runtime round-trip failed: got %q", got.Runtime)
	}
}

func TestVerifyDockerNetwork_MissingNetworkErrors(t *testing.T) {
	fd := &fakeDocker{errs: map[string]error{"network": errors.New("Error: No such network: myproj_appnet")}}
	err := verifyDockerNetwork(context.Background(), fd.runner(), "myproj_appnet")
	if err == nil {
		t.Fatal("expected an error for a missing network")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("code = %v, want INVALID_ARGUMENT", phelixerr.CodeOf(err))
	}
	// The one docker call made was the inspect probe — nothing was built or run.
	if len(fd.calls) != 1 || fd.calls[0][0] != "network" {
		t.Fatalf("expected a single `docker network inspect` call, got %v", fd.calls)
	}
}

func TestVerifyDockerNetwork_ExistingNetworkProceeds(t *testing.T) {
	fd := &fakeDocker{} // default runner returns success for `network inspect`
	if err := verifyDockerNetwork(context.Background(), fd.runner(), "myproj_appnet"); err != nil {
		t.Fatalf("existing network must pass, got %v", err)
	}
}

func TestVerifyDockerNetwork_EmptyIsNoop(t *testing.T) {
	fd := &fakeDocker{}
	if err := verifyDockerNetwork(context.Background(), fd.runner(), ""); err != nil {
		t.Fatalf("empty network must be a no-op, got %v", err)
	}
	if len(fd.calls) != 0 {
		t.Fatalf("empty network must not touch docker, got %v", fd.calls)
	}
}

func TestDockerLauncher_AttachesToNetwork(t *testing.T) {
	fd := &fakeDocker{run: "cid\n", inspect: "51000\n"}
	launcher := newDockerLauncher("billing", "blue", "myproj_appnet", fd.runner())

	if _, _, err := launcher(context.Background(), "billing:v3", nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	run := fd.calls[0]
	joined := strings.Join(run, " ")
	// The network attach is present, additive to the loopback publish, and the
	// image reference is still the last argument.
	if !strings.Contains(joined, "--network myproj_appnet") {
		t.Errorf("expected --network myproj_appnet, got %v", run)
	}
	if !strings.Contains(joined, "127.0.0.1::") {
		t.Errorf("loopback publish must remain when attaching to a network: %v", run)
	}
	if strings.Contains(joined, "--network-alias") {
		t.Errorf("--network-alias must never be set (blue-green runs two instances): %v", run)
	}
	if run[len(run)-1] != "billing:v3" {
		t.Errorf("image ref must stay the last arg, got %q", run[len(run)-1])
	}
}

func TestDockerLauncher_EmptyImageRejected(t *testing.T) {
	fd := &fakeDocker{}
	launcher := newDockerLauncher("app", "", "", fd.runner())
	if _, _, err := launcher(context.Background(), "", nil); err == nil {
		t.Fatal("expected error for empty image ref")
	} else if phelixerr.CodeOf(err) != phelixerr.CodeInvalidArgument {
		t.Fatalf("code = %v, want INVALID_ARGUMENT", phelixerr.CodeOf(err))
	}
}

func TestDockerLauncher_RunFailureIsInstanceStartFailed(t *testing.T) {
	fd := &fakeDocker{errs: map[string]error{"run": errors.New("boom")}}
	launcher := newDockerLauncher("app", "", "", fd.runner())
	_, _, err := launcher(context.Background(), "app:v1", nil)
	if err == nil {
		t.Fatal("expected launch error")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeInstanceStartFailed {
		t.Fatalf("code = %v, want INSTANCE_START_FAILED", phelixerr.CodeOf(err))
	}
}

func TestDockerLauncher_NoPortKillsContainer(t *testing.T) {
	// Container starts, but the port cannot be resolved → the half-started
	// container must be killed so nothing leaks.
	fd := &fakeDocker{run: "cid999\n", errs: map[string]error{"inspect": errors.New("no ports")}}
	launcher := newDockerLauncher("app", "", "", fd.runner())
	if _, _, err := launcher(context.Background(), "app:v1", nil); err == nil {
		t.Fatal("expected error when port cannot be resolved")
	}
	// A kill call for cid999 must have been issued.
	killed := false
	for _, c := range fd.calls {
		if len(c) >= 2 && c[0] == "kill" && c[len(c)-1] == "cid999" {
			killed = true
		}
	}
	if !killed {
		t.Fatalf("half-started container was not killed: %v", fd.calls)
	}
}

func TestDockerProcess_WaitNonZeroExitIsError(t *testing.T) {
	fd := &fakeDocker{wait: "137\n"}
	p := &dockerProcess{id: "cid", run: fd.runner()}
	err := p.Wait()
	if err == nil {
		t.Fatal("expected non-nil error for exit code 137")
	}
	if phelixerr.CodeOf(err) != phelixerr.CodeProcessFailed {
		t.Fatalf("code = %v, want PROCESS_FAILED", phelixerr.CodeOf(err))
	}
}

func TestDockerProcess_WaitZeroExitIsNil(t *testing.T) {
	fd := &fakeDocker{wait: "0\n"}
	p := &dockerProcess{id: "cid", run: fd.runner()}
	if err := p.Wait(); err != nil {
		t.Fatalf("clean exit should be nil, got %v", err)
	}
}

func TestDockerProcess_SignalSendsSignalNumber(t *testing.T) {
	fd := &fakeDocker{}
	p := &dockerProcess{id: "cid", run: fd.runner()}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	last := fd.calls[len(fd.calls)-1]
	joined := strings.Join(last, " ")
	if !strings.Contains(joined, "kill") || !strings.Contains(joined, "--signal=15") {
		t.Fatalf("expected docker kill --signal=15, got %v", last)
	}
}

func TestDockerProcess_GoneContainerTreatedAsDone(t *testing.T) {
	fd := &fakeDocker{errs: map[string]error{"kill": errors.New("Error: No such container: cid")}}
	p := &dockerProcess{id: "cid", run: fd.runner()}
	if err := p.Kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("gone container Kill should be ErrProcessDone, got %v", err)
	}
}

func TestGracefulStop_DrivesDockerProcess(t *testing.T) {
	// A dockerProcess whose Wait returns promptly should stop cleanly through
	// the shared GracefulStop path (proving the Process contract holds).
	fd := &fakeDocker{wait: "0\n"}
	p := &dockerProcess{id: "cid", run: fd.runner()}
	report, err := GracefulStop(context.Background(), p, 2*time.Second, 0)
	if err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	if !report.Exited {
		t.Fatal("report should mark the instance exited")
	}
}
