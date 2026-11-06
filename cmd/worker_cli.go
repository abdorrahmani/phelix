package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/fatih/color"
)

// workerAppState loads the deploy state when the app is a ModeWorker app,
// nil otherwise (the zero-downtime and classic paths are untouched).
func workerAppState(name string) (*deploy.DeployState, error) {
	state, err := deploy.Load(name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if state == nil || state.Mode != deploy.ModeWorker {
		return nil, nil
	}
	return state, nil
}

// workerStopSettings resolves the configured shutdown signal and grace period
// for one worker app's CLI operations. A missing config falls back to the
// documented defaults.
func workerStopSettings(appInfo *app.AppInfo) (os.Signal, time.Duration, error) {
	cfg, err := loadProjectConfigFrom(appInfo.Directory)
	if err != nil && phelixerr.CodeOf(err) != phelixerr.CodeNotFound {
		return nil, 0, err
	}
	if cfg == nil {
		cfg = &project.Config{}
	}
	ps := cfg.Process.Resolve()
	sig, err := deploy.SignalFromName(ps.ShutdownSignal)
	if err != nil {
		return nil, 0, err
	}
	return sig, ps.GracePeriod, nil
}

// syncAppInfoWithWorker projects worker replica liveness onto the app record:
// running when every desired replica is alive, degraded when some are not,
// stopped when none are. Same write discipline as syncAppInfoWithDeploy.
func syncAppInfoWithWorker(info *app.AppInfo, state *deploy.DeployState) error {
	if info == nil || state == nil {
		return nil
	}
	am, ok := app.Manager.(*app.AppManager)
	if !ok {
		return nil
	}
	summary := deploy.SummarizeWorker(state)
	before := *info
	info.StartTicks = 0
	switch summary.Status {
	case "running", "degraded":
		info.Status = summary.Status
		info.PID = firstAliveWorkerPID(state)
		info.AutoStart = true
		if inst := firstAliveWorkerInstance(state); inst != nil {
			info.Start = inst.StartedAt
			info.StartTicks = inst.StartTicks
		}
	default:
		info.Status = "stopped"
		info.PID = 0
	}
	if info.Status == before.Status && info.PID == before.PID && info.StartTicks == before.StartTicks {
		return nil
	}
	info.UpdatedAt = time.Now()
	return am.SaveState()
}

// firstAliveWorkerInstance returns the lowest-slot alive replica record.
func firstAliveWorkerInstance(state *deploy.DeployState) *deploy.Instance {
	keys := make([]string, 0, len(state.Replicas))
	for k := range state.Replicas {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i])
		b, _ := strconv.Atoi(keys[j])
		return a < b
	})
	for _, k := range keys {
		if inst := state.Replicas[k]; inst != nil && deploy.WorkerInstanceAlive(inst) {
			return inst
		}
	}
	return nil
}

// firstAliveWorkerPID returns the lowest-slot alive replica's PID, or 0.
func firstAliveWorkerPID(state *deploy.DeployState) int {
	if inst := firstAliveWorkerInstance(state); inst != nil {
		return inst.PID
	}
	return 0
}

// runWorkerStop stops every replica of a worker app.
func runWorkerStop(appInfo *app.AppInfo) error {
	sig, grace, err := workerStopSettings(appInfo)
	if err != nil {
		return err
	}
	stopped, err := deploy.WorkerStop(context.Background(), appInfo.Name, sig, grace)
	if err != nil {
		return err
	}
	am, _ := app.Manager.(*app.AppManager)
	if info := am.Apps[appInfo.ID]; info != nil {
		info.Status = "stopped"
		info.PID = 0
		info.AutoStart = false
		info.UpdatedAt = time.Now()
		_ = am.SaveState()
	}
	fmt.Printf("  %s %d worker replica(s) stopped\n", color.GreenString("✓"), stopped)
	return nil
}

// runWorkerStart launches every dead replica slot of a worker app.
func runWorkerStart(appInfo *app.AppInfo) error {
	sig, grace, err := workerStopSettings(appInfo)
	if err != nil {
		return err
	}
	launched, err := deploy.WorkerStart(context.Background(), appInfo.Name, sig, grace)
	if err != nil {
		return err
	}
	if launched == 0 {
		fmt.Printf("%s Worker %s replicas are already running\n",
			color.GreenString("✓"), color.CyanString("'%s'", appInfo.Name))
		return nil
	}
	reconcileAppWithDeploy(appInfo.Name)
	fmt.Printf("%s Worker %s started (%d replica(s) launched)\n",
		color.GreenString("✓"), color.CyanString("'%s'", appInfo.Name), launched)
	return nil
}

// runWorkerRestart restarts one replica (--replica) or every replica of a
// worker app, resetting crash counters.
func runWorkerRestart(appInfo *app.AppInfo, replica string) error {
	sig, grace, err := workerStopSettings(appInfo)
	if err != nil {
		return err
	}
	if replica != "" {
		if err := deploy.WorkerRestartReplica(context.Background(), appInfo.Name, replica, sig, grace); err != nil {
			return err
		}
		reconcileAppWithDeploy(appInfo.Name)
		fmt.Printf("%s Replica %s of %s restarted\n",
			color.GreenString("✓"), color.CyanString(replica), color.CyanString("'%s'", appInfo.Name))
		return nil
	}
	count, err := deploy.WorkerRestartAll(context.Background(), appInfo.Name, sig, grace)
	if err != nil {
		return err
	}
	reconcileAppWithDeploy(appInfo.Name)
	fmt.Printf("%s %d worker replica(s) restarted\n", color.GreenString("✓"), count)
	return nil
}

// runWorkerTeardown stops every replica before app removal.
func runWorkerTeardown(appInfo *app.AppInfo) error {
	sig, grace, err := workerStopSettings(appInfo)
	if err != nil {
		return err
	}
	stopped, err := deploy.WorkerStop(context.Background(), appInfo.Name, sig, grace)
	if err != nil {
		return err
	}
	fmt.Printf("  %s %d worker replica(s) stopped before removal\n", color.GreenString("✓"), stopped)
	return nil
}
