package monitor

import (
	"os"
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
	if err != nil || state == nil || state.Mode != deploy.ModeWorker {
		if version, verr := deploy.CurrentVersion(a.Name); verr == nil && version > 0 {
			base.Version = "v" + strconv.Itoa(version)
		}
		return []logs.AppLogTarget{base}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	slots, err := deploy.WorkerReplicaSlots(a.Name)
	if err != nil {
		return nil
	}
	out := make([]logs.AppLogTarget, 0, len(slots))
	for _, slot := range slots {
		inst := state.Replicas[slot]
		if inst == nil || inst.BinaryPath == "" {
			continue
		}
		target := base
		target.Path = deploy.WorkerLogFilePath(home, inst.BinaryPath, slot)
		target.Replica = slot
		if inst.Version > 0 {
			target.Version = "v" + strconv.Itoa(inst.Version)
		}
		out = append(out, target)
	}
	return out
}
