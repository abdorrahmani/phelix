package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/abdorrahmani/phelix/internal/logs"

	"github.com/abdorrahmani/phelix/internal/app"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/abdorrahmani/phelix/internal/resources"
)

// WorkerLauncher starts one worker replica. Unlike InstanceLauncher it
// allocates no port and injects no PORT variable: a worker is judged by its
// process identity and health policy, never by a listener. Tests inject a
// fake. The returned identity is captured at spawn and is the PID-reuse-safe
// fingerprint the supervisor and engine verify later.
type WorkerLauncher func(binaryPath string, env []string, replica string) (Process, processex.ProcessIdentity, error)

// WorkerLauncherForApp returns the native worker launcher with the app's
// configured per-replica resource limits. Workers are native-runtime only in
// v1; docker workers are rejected at config validation.
func WorkerLauncherForApp(appName string) WorkerLauncher {
	return func(binaryPath string, env []string, replica string) (Process, processex.ProcessIdentity, error) {
		cfg, err := app.LoadResources(appName)
		if err != nil {
			return nil, processex.ProcessIdentity{}, phelixerr.Wrap(phelixerr.CodeConfiguration, "worker: load resource limits", err)
		}
		return launchWorkerInstance(binaryPath, env, cfg, replica)
	}
}

// launchWorkerInstance execs the built binary without any port plumbing. The
// structure mirrors launchInstance: parent env plus the caller overlay (no
// PORT appended), per-replica log file, cgroup lease via resources.Start, and
// one wait goroutine that classifies a cgroup OOM before releasing the lease.
func launchWorkerInstance(binaryPath string, env []string, cfg resources.Config, replica string) (Process, processex.ProcessIdentity, error) {
	if binaryPath == "" {
		return nil, processex.ProcessIdentity{}, phelixerr.New(phelixerr.CodeInvalidArgument, "worker: binary path is empty")
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return nil, processex.ProcessIdentity{}, phelixerr.Wrapf(phelixerr.CodeNotFound, err, "worker: binary not found")
	}
	if replica == "" {
		return nil, processex.ProcessIdentity{}, phelixerr.New(phelixerr.CodeInvalidArgument, "worker: replica name is empty")
	}

	logFile, err := openWorkerInstanceLog(binaryPath, replica)
	if err != nil {
		return nil, processex.ProcessIdentity{}, err
	}

	cmd := exec.Command(binaryPath)
	cmd.Env = workerEnv(env)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	closeDrain, err := logs.AttachDrain(cmd, logFile)
	if err != nil {
		logFile.Close()
		return nil, processex.ProcessIdentity{}, err
	}
	defer closeDrain()
	releaseSecrets := phelixerr.AcquireSecretValues(phelixerr.SecretValuesInEnv(cmd.Env))
	inst, err := resources.Start(cmd, cfg)
	if err != nil {
		releaseSecrets()
		_ = logFile.Close()
		return nil, processex.ProcessIdentity{}, phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "worker: start replica %s", replica)
	}
	pid := cmd.Process.Pid
	memoryLimit := cfg.Memory

	identity, err := processex.Identity(pid)
	if err != nil {
		releaseSecrets()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if inst != nil {
			_ = inst.Close()
		}
		_ = logFile.Close()
		return nil, processex.ProcessIdentity{}, phelixerr.Wrapf(phelixerr.CodeInstanceStartFailed, err, "worker: capture identity of replica %s", replica)
	}

	errCh := make(chan struct{})
	proc := &osProcess{cmd: cmd, pid: pid, startTicks: identity.StartTime, doneCh: errCh}
	go func() {
		defer releaseSecrets()
		waitErr := cmd.Wait()
		if inst != nil {
			oom, oomErr := inst.ResourceOOM()
			_ = inst.Close()
			if oomErr == nil && oom {
				waitErr = oomExitError(waitErr, pid, memoryLimit)
			}
		}
		_ = logFile.Close()
		proc.reap(waitErr)
	}()

	return proc, identity, nil
}

// openWorkerInstanceLog creates (or appends to) the per-replica log file. A
// worker replica has no port, so the replica name names the file instead of
// the internal port a service instance uses.
func openWorkerInstanceLog(binaryPath string, replica string) (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.OpenFile(os.DevNull, os.O_RDWR, 0)
	}
	dir := filepath.Join(home, ".phelix", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return os.OpenFile(os.DevNull, os.O_RDWR, 0)
	}
	return os.OpenFile(workerLogPath(home, binaryPath, replica), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// workerLogPath is the on-disk location of one worker replica's output.
func workerLogPath(home, binaryPath, replica string) string {
	name := fmt.Sprintf("deploy_%s_%s.log", filepath.Base(binaryPath), workerLogTag(replica))
	return filepath.Join(home, ".phelix", "logs", name)
}

// WorkerLogFilePath returns the on-disk log path of one worker replica, for
// CLI surfaces that tail replica output directly.
func WorkerLogFilePath(home, binaryPath, replica string) string {
	return workerLogPath(home, binaryPath, replica)
}

// workerLogTag renders a replica slot ("0") as its log-friendly tag
// ("replica-0"), matching the identity the supervisor and status use.
func workerLogTag(replica string) string {
	return "replica-" + replica
}

// workerEnv keeps worker processes and their probes free of service ports.
func workerEnv(overlay []string) []string {
	return slices.DeleteFunc(mergeByKey(os.Environ(), overlay), func(entry string) bool {
		return strings.HasPrefix(entry, "PORT=")
	})
}
