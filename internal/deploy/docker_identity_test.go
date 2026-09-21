package deploy

import (
	"context"
	"errors"
	"testing"
)

// fakeIdentityDocker scripts `docker inspect` output for the identity runner.
type fakeIdentityDocker struct {
	inspectOut string
	inspectErr error
	calls      [][]string
}

func (f *fakeIdentityDocker) runner() dockerRunner {
	return func(_ context.Context, args ...string) (string, error) {
		f.calls = append(f.calls, args)
		if f.inspectErr != nil {
			return "", f.inspectErr
		}
		return f.inspectOut, nil
	}
}

func withIdentityRunner(t *testing.T, r dockerRunner) {
	t.Helper()
	prev := dockerIdentityRunner
	dockerIdentityRunner = r
	t.Cleanup(func() { dockerIdentityRunner = prev })
}

func TestDockerContainerAlive_RunningManaged(t *testing.T) {
	fd := &fakeIdentityDocker{inspectOut: "true|true\n"}
	withIdentityRunner(t, fd.runner())
	if !dockerContainerAlive("cid") {
		t.Fatal("a running, phelix.managed container must be alive")
	}
}

func TestDockerContainerAlive_StoppedIsDead(t *testing.T) {
	fd := &fakeIdentityDocker{inspectOut: "false|true\n"}
	withIdentityRunner(t, fd.runner())
	if dockerContainerAlive("cid") {
		t.Fatal("a stopped container must not be alive")
	}
}

func TestDockerContainerAlive_UnmanagedIsRefused(t *testing.T) {
	// Running but NOT phelix.managed — the container-runtime analogue of a
	// recycled PID pointing at an unrelated process. Must be refused.
	fd := &fakeIdentityDocker{inspectOut: "true|\n"}
	withIdentityRunner(t, fd.runner())
	if dockerContainerAlive("cid") {
		t.Fatal("an unmanaged container must never be treated as a live phelix instance")
	}
}

func TestDockerContainerAlive_GoneContainer(t *testing.T) {
	fd := &fakeIdentityDocker{inspectErr: errors.New("No such container: cid")}
	withIdentityRunner(t, fd.runner())
	if dockerContainerAlive("cid") {
		t.Fatal("a missing container must not be alive")
	}
}

func TestInstanceAlive_RoutesDockerInstancesThroughDocker(t *testing.T) {
	// A docker instance has a bogus PID field (a host PID we never signal) and
	// an image ref in BinaryPath. The native check would try to match the image
	// ref against an executable path and fail; the docker check must be used.
	fd := &fakeIdentityDocker{inspectOut: "true|true\n"}
	withIdentityRunner(t, fd.runner())
	inst := &Instance{Slot: "blue", ContainerID: "cid", BinaryPath: "billing:v3", PID: 999999}
	if !InstanceAlive(inst) {
		t.Fatal("docker instance must be judged alive via Docker, not the executable check")
	}
	// It must have asked docker, not walked the process table.
	if len(fd.calls) == 0 {
		t.Fatal("expected an inspect call for the docker instance")
	}
}

func TestInstanceAlive_NativeInstanceUnaffected(t *testing.T) {
	// A native instance (no ContainerID) with a dead PID stays dead; the docker
	// runner must not even be consulted.
	fd := &fakeIdentityDocker{inspectErr: errors.New("should not be called")}
	withIdentityRunner(t, fd.runner())
	inst := &Instance{Slot: "blue", PID: 0}
	if InstanceAlive(inst) {
		t.Fatal("a native instance with pid 0 must be dead")
	}
	if len(fd.calls) != 0 {
		t.Fatal("native path must not consult docker")
	}
}

func TestStopInstance_DockerRoutesToContainerStop(t *testing.T) {
	// A docker instance stop must verify the container is alive+managed, then
	// drive GracefulStop through the container. Script: inspect(alive) then wait.
	var calls [][]string
	run := func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, args)
		switch args[0] {
		case "inspect":
			return "true|true\n", nil
		case "wait":
			return "0\n", nil
		default:
			return "", nil
		}
	}
	withIdentityRunner(t, run)
	inst := &Instance{Slot: "blue", ContainerID: "cid"}
	report := stopInstance(context.Background(), inst, 2_000_000_000, 0)
	if !report.Exited {
		t.Fatal("docker instance should stop cleanly")
	}
}

func TestRollbackSourceForRuntime_PicksByRuntime(t *testing.T) {
	if _, ok := RollbackSourceForRuntime(RuntimeDocker, "app", "id", 3).(*DockerVersionSource); !ok {
		t.Fatal("docker runtime must resolve a DockerVersionSource")
	}
	if _, ok := RollbackSourceForRuntime(RuntimeNative, "app", "id", 3).(*ExistingVersionSource); !ok {
		t.Fatal("native runtime must resolve an ExistingVersionSource")
	}
	// Empty runtime (every pre-docker state) defaults to native.
	if _, ok := RollbackSourceForRuntime("", "app", "id", 3).(*ExistingVersionSource); !ok {
		t.Fatal("empty runtime must default to the native source")
	}
}

func TestLauncherForRuntime_PicksByRuntime(t *testing.T) {
	// Smoke check: both return non-nil launchers; the docker branch must not
	// panic constructing its closure.
	if LauncherForRuntime(RuntimeDocker, "app") == nil {
		t.Fatal("docker runtime launcher is nil")
	}
	if LauncherForRuntime(RuntimeNative, "app") == nil {
		t.Fatal("native runtime launcher is nil")
	}
}
