package deploy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/processex"
)

// Worker replica lifecycle operations shared by the supervisor, the deploy
// engine and the CLI. Every mutation of a worker replica record flows through
// here so the persisted record (identity, status, exit bookkeeping) stays
// consistent regardless of who drives it.

// WorkerInstanceAlive identity-checks a worker replica record: the recorded
// PID must belong to a live process with the recorded executable and kernel
// start time.
func WorkerInstanceAlive(inst *Instance) bool {
	if inst == nil || inst.PID <= 0 {
		return false
	}
	return processex.Matches(processex.ProcessIdentity{PID: inst.PID, StartTime: inst.StartTicks, ExePath: inst.BinaryPath})
}

// workerLauncherFactory is the spawn seam for worker replicas; tests override
// it to launch helper processes with their own environment.
var workerLauncherFactory = WorkerLauncherForApp

// LaunchWorkerReplica spawns the slot's replica and persists the fresh record.
// The replica inherits the slot's binary, version and env snapshot. It starts
// in status "starting"; whoever launched it (supervisor, deploy engine or CLI)
// moves it to running once the startup gate passes. Restart accounting is the
// caller's decision, not the launcher's.
func LaunchWorkerReplica(appName string, state *DeployState, slot string) (Process, error) {
	inst := state.Replicas[slot]
	if inst == nil {
		inst = &Instance{Slot: slot}
		state.Replicas[slot] = inst
	}
	if inst.BinaryPath == "" {
		return nil, phelixerr.Newf(phelixerr.CodeConfiguration, "worker: replica %s has no binary recorded (deploy first)", slot)
	}
	env, err := EnvOverlayFromSnapshot(inst.EnvPath, state.AppID)
	if err != nil {
		return nil, err
	}
	proc, id, err := workerLauncherFactory(appName)(inst.BinaryPath, env, slot)
	if err != nil {
		return nil, err
	}
	MarkWorkerLaunched(state, slot, id)
	return proc, nil
}

// MarkWorkerLaunched stamps a replica record as freshly launched and persists
// it: new identity, status starting, exit bookkeeping cleared.
func MarkWorkerLaunched(state *DeployState, slot string, id processex.ProcessIdentity) {
	inst := state.Replicas[slot]
	if inst == nil {
		inst = &Instance{Slot: slot}
		state.Replicas[slot] = inst
	}
	now := time.Now()
	inst.PID = id.PID
	inst.StartTicks = id.StartTime
	inst.StartedAt = now
	inst.ReadyAt = time.Time{}
	inst.LastExitCode = 0
	inst.LastExitReason = ""
	inst.LastExitAt = time.Time{}
	inst.Status = "starting"
	_ = Store(state)
}

// GracefulStopWorker stops one identity-verified worker process without
// touching its record: signal, wait within grace, SIGKILL escalation, reap.
// The caller owns all status transitions. held is the launcher's Process when
// the caller spawned the replica; otherwise the process is adopted by PID
// after identity verification. The returned waitErr carries the exit status
// for exit bookkeeping.
func GracefulStopWorker(inst *Instance, sig os.Signal, grace time.Duration, held Process) (report ShutdownReport, waitErr error, stopErr error) {
	if !WorkerInstanceAlive(inst) {
		return ShutdownReport{Exited: true}, nil, nil
	}
	proc := held
	if proc == nil {
		p, err := os.FindProcess(inst.PID)
		if err != nil {
			return ShutdownReport{}, nil, phelixerr.Wrapf(phelixerr.CodeProcessFailed, err, "worker: adopt pid %d", inst.PID)
		}
		proc = &pidProcess{pid: inst.PID, proc: p}
	}
	report, stopErr = GracefulStopSignal(sig, context.Background(), proc, grace, -1)
	waitErr = proc.Wait()
	return report, waitErr, stopErr
}

// StopWorkerReplica gracefully stops the slot's replica: signal, wait within
// grace, SIGKILL escalation, reap. The process handle of a supervisor-spawned
// replica can be passed as held; otherwise the process is adopted by PID with
// identity verification first. The record ends in status "stopped" with the
// exit recorded.
func StopWorkerReplica(state *DeployState, slot string, sig os.Signal, grace time.Duration, held Process) (ShutdownReport, error) {
	inst := state.Replicas[slot]
	if inst == nil {
		return ShutdownReport{Exited: true}, nil
	}
	if !WorkerInstanceAlive(inst) {
		inst.Status = "stopped"
		_ = Store(state)
		return ShutdownReport{Exited: true}, nil
	}
	inst.Status = "stopping"
	_ = Store(state)
	report, waitErr, stopErr := GracefulStopWorker(inst, sig, grace, held)
	inst.Status = "stopped"
	inst.LastExitAt = time.Now()
	code, reason := exitCodeOf(waitErr)
	inst.LastExitCode = code
	inst.LastExitReason = "stopped by phelix: " + reason
	_ = Store(state)
	return report, stopErr
}

// RecordInstanceExit writes the exit bookkeeping of a dead replica onto its
// record. adopted marks a process this process never spawned (no waitable
// handle), whose exit code is genuinely unknowable.
func RecordInstanceExit(inst *Instance, waitErr error, adopted bool) {
	inst.LastExitAt = time.Now()
	if adopted {
		inst.LastExitCode = -1
		inst.LastExitReason = "unknown (process not supervised from spawn)"
		return
	}
	code, reason := exitCodeOf(waitErr)
	inst.LastExitCode = code
	inst.LastExitReason = reason
}

// exitCodeOf extracts the exit status from a wait error. A cgroup OOM kill is
// reported with its own reason so restart accounting and status can show it.
func exitCodeOf(waitErr error) (code int, reason string) {
	if waitErr == nil {
		return 0, "clean exit"
	}
	if phelixerr.CodeOf(waitErr) == phelixerr.CodeResourceOOM {
		return -1, "resource OOM"
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode(), waitErr.Error()
	}
	return -1, waitErr.Error()
}

// SignalFromName maps a configured shutdown signal name to its syscall value.
func SignalFromName(name string) (syscall.Signal, error) {
	switch name {
	case "SIGTERM":
		return syscall.SIGTERM, nil
	case "SIGINT":
		return syscall.SIGINT, nil
	}
	return 0, phelixerr.Newf(phelixerr.CodeConfiguration, "unsupported shutdown signal %q (expected SIGTERM or SIGINT)", name)
}
