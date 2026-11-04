package monitor

import (
	"os"
	"sort"
	"strconv"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	"github.com/abdorrahmani/phelix/internal/logs"
)

// AppLogTargets resolves the log files one app writes. A service writes one
// combined file under its app ID; a worker writes one file per replica, so
// each replica becomes its own target and carries its slot identifier.
func AppLogTargets(a app.AppListItem) []logs.AppLogTarget {
	base := a.LogTarget()
	state, err := deploy.Load(a.Name)
	if err != nil || state == nil || state.Mode == "classic" {
		if version, verr := deploy.CurrentVersion(a.Name); verr == nil && version > 0 {
			base.Version = "v" + strconv.Itoa(version)
		}
		return []logs.AppLogTarget{base}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	instances := state.Replicas
	if state.Mode == deploy.ModeBlueGreen {
		instances = state.Slots
	}
	slots := make([]string, 0, len(instances))
	for slot := range instances {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	out := make([]logs.AppLogTarget, 0, len(slots))
	for _, slot := range slots {
		inst := instances[slot]
		if inst == nil || inst.BinaryPath == "" {
			continue
		}
		target := base
		if state.Mode == deploy.ModeWorker {
			target.Path = deploy.WorkerLogFilePath(home, inst.BinaryPath, slot)
		} else {
			if inst.Port <= 0 {
				continue
			}
			target.Path = deploy.InstanceLogFilePath(home, inst.BinaryPath, inst.Port)
		}
		target.Replica = slot
		if inst.Version > 0 {
			target.Version = "v" + strconv.Itoa(inst.Version)
		}
		out = append(out, target)
	}
	return out
}
