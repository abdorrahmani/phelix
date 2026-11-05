package deploy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/processex"
	"github.com/shirou/gopsutil/process"
)

// reclaimSecretLaunches stops tagged launches that the journal never accepted.
func reclaimSecretLaunches(appName, txnID string, accepted *DeployState) error {
	keepPIDs := map[int]bool{}
	keepContainers := map[string]bool{}
	if accepted != nil {
		for _, group := range []map[string]*Instance{accepted.Slots, accepted.Replicas} {
			for _, inst := range group {
				if inst == nil || inst.Status != "running" && inst.Status != "replacing" {
					continue
				}
				keepPIDs[inst.PID] = true
				if inst.ContainerID != "" {
					keepContainers[inst.ContainerID] = true
				}
			}
		}
	}
	procs, err := process.Processes()
	if err != nil {
		return phelixerr.New(phelixerr.CodeProcessFailed, "cannot inspect interrupted secret launches")
	}
	for _, proc := range procs {
		if keepPIDs[int(proc.Pid)] {
			continue
		}
		entries, err := proc.Environ()
		if err != nil {
			continue
		}
		tagged := false
		for _, entry := range entries {
			if entry == "PHELIX_INTERNAL_SECRET_TXN="+txnID {
				tagged = true
				break
			}
		}
		if !tagged {
			continue
		}
		parent, err := proc.Ppid()
		for err == nil && parent > 1 && !keepPIDs[int(parent)] {
			ancestor, lookupErr := process.NewProcess(parent)
			if lookupErr != nil {
				break
			}
			parent, err = ancestor.Ppid()
		}
		if keepPIDs[int(parent)] {
			continue
		}
		identity, err := processex.Identity(int(proc.Pid))
		if err != nil {
			continue
		}
		if processex.Matches(identity) {
			if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
				return phelixerr.New(phelixerr.CodeProcessFailed, "cannot reclaim interrupted secret launch")
			}
		}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := execDockerRunner(ctx, "ps", "-a", "--no-trunc", "--filter", "label="+DockerLabelApp+"="+appName, "--filter", "label=phelix.secret-txn="+txnID, "--format", "{{.ID}}")
	if err != nil {
		if accepted == nil || accepted.Runtime != RuntimeDocker {
			return nil
		}
		return err
	}
	for _, id := range strings.Fields(out) {
		if !keepContainers[id] {
			if _, err := execDockerRunner(ctx, "rm", "-f", id); err != nil {
				return err
			}
		}
	}
	return nil
}

// secretLaunchTxn reads the non-secret launch marker without copying credentials.
func secretLaunchTxn(env []string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PHELIX_INTERNAL_SECRET_TXN="); ok {
			return value
		}
	}
	return ""
}
