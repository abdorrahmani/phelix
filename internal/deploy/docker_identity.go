package deploy

import (
	"context"
	"strings"
	"time"
)

// dockerInspector reads container state for identity verification. It is the
// same seam shape as dockerRunner; production uses execDockerRunner. Kept as a
// package var so tests can substitute a fake without a daemon and without
// threading a runner through InstanceAlive's signature (which the native path
// shares).
var dockerIdentityRunner dockerRunner = execDockerRunner

// dockerContainerAlive reports whether containerID names a running container
// that Phelix manages (carries the phelix.managed label). Checking the label
// is the container-runtime analogue of findVerifiedProcess's executable check:
// it refuses to treat an unrelated container — or a container Phelix did not
// create — as a live managed instance. A container id is content-addressed and
// never recycled, so unlike a PID it cannot silently point at a different
// workload later; the label check is belt-and-suspenders against a manually
// re-created id collision.
func dockerContainerAlive(containerID string) bool {
	if containerID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := dockerIdentityRunner(ctx,
		"inspect", "--format", "{{.State.Running}}|{{index .Config.Labels \""+DockerLabelManaged+"\"}}", containerID)
	if err != nil {
		// No such container / daemon down → not a live managed instance.
		return false
	}
	running, managed, ok := strings.Cut(strings.TrimSpace(out), "|")
	if !ok {
		return false
	}
	return running == "true" && managed == "true"
}

// dockerProcessByID rebuilds a Process handle for a container started by a
// previous CLI invocation (whose in-memory handle we no longer hold), the
// container-runtime analogue of findVerifiedProcess → pidProcess. It returns
// nil when the container is not a live managed instance, so a stale state
// record can never drive a stop against an unrelated container.
func dockerProcessByID(containerID string) Process {
	if !dockerContainerAlive(containerID) {
		return nil
	}
	return &dockerProcess{id: containerID, run: dockerIdentityRunner}
}

// stopByContainerID gracefully stops a container instance from a previous
// invocation, verifying first that the id still names a live managed container.
// Returns Exited=true when nothing needed stopping (already gone, or the record
// was stale so an unrelated container was protected) — matching stopByPID's
// contract so the deploy flow treats both runtimes identically.
func stopByContainerID(ctx context.Context, containerID string, grace time.Duration, inFlight int64) ShutdownReport {
	proc := dockerProcessByID(containerID)
	if proc == nil {
		return ShutdownReport{Exited: true, InFlight: inFlight}
	}
	report, _ := GracefulStop(ctx, proc, grace, inFlight)
	return report
}

// instanceAliveViaRuntime is the runtime-aware liveness check InstanceAlive
// delegates to: docker instances (ContainerID set) verify through Docker;
// everything else keeps the exact PID+executable check the native path always
// used. Centralizing the branch here means every caller of InstanceAlive
// (ServingInstance, resync, lifecycle, rollout recovery) becomes container-aware
// at once — the invariant is upheld uniformly, never half-migrated.
func instanceAliveViaRuntime(inst *Instance) bool {
	if inst == nil {
		return false
	}
	if inst.ContainerID != "" {
		return dockerContainerAlive(inst.ContainerID)
	}
	if inst.PID <= 0 {
		return false
	}
	return findVerifiedProcess(inst.PID, inst.BinaryPath) != nil
}

// stopInstance stops one instance using whichever runtime it was started with,
// so blue-green/rolling drain paths call a single helper instead of branching
// at every site. It preserves stopByPID's "verify identity before signalling"
// guarantee for both runtimes.
func stopInstance(ctx context.Context, inst *Instance, grace time.Duration, inFlight int64) ShutdownReport {
	if inst == nil {
		return ShutdownReport{Exited: true, InFlight: inFlight}
	}
	if inst.ContainerID != "" {
		return stopByContainerID(ctx, inst.ContainerID, grace, inFlight)
	}
	return stopByPID(ctx, inst.PID, grace, inFlight, inst.BinaryPath)
}
