package deploy

import (
	"os"
	"sort"
	"strconv"
)

// DetectWorkerOrphans scans /proc for processes running one of the app's
// recorded worker binaries whose PID is not recorded in the deploy state.
// Orphans appear when a replica outlived its record (a lost deploy.json, a
// crash between spawn and persist). Detection is report-only: killing an
// unidentified process is a human decision, surfaced by `phelix status` and
// the daemon log.
//
// shortcut: only binaries referenced by current replica records are matched;
// a stray process from a pruned version is not detected.
func DetectWorkerOrphans(state *DeployState) []int {
	if state == nil || state.Mode != ModeWorker {
		return nil
	}
	exes := make(map[string]bool)
	recorded := make(map[int]bool)
	for _, inst := range state.Replicas {
		if inst == nil {
			continue
		}
		if inst.BinaryPath != "" {
			exes[inst.BinaryPath] = true
		}
		if inst.PID > 0 {
			recorded[inst.PID] = true
		}
	}
	if len(exes) == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || recorded[pid] {
			continue
		}
		exe, err := os.Readlink("/proc/" + entry.Name() + "/exe")
		if err != nil {
			continue
		}
		for want := range exes {
			if sameExecutable(exe, want) {
				pids = append(pids, pid)
				break
			}
		}
	}
	sort.Ints(pids)
	return pids
}
