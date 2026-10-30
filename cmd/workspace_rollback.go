package cmd

import (
	"fmt"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/authz"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	"github.com/abdorrahmani/phelix/internal/workspace"
	"github.com/fatih/color"
)

// wsRollbackAll switches `phelix rollback` into whole-workspace mode.
var wsRollbackAll bool

type workspaceRollbackResult struct {
	Workspace string                         `json:"workspace"`
	Source    string                         `json:"source"`
	Services  []workspaceRollbackServiceView `json:"services"`
}

type workspaceRollbackServiceView struct {
	Name        string `json:"name"`
	FromVersion int    `json:"from_version,omitempty"`
	ToVersion   int    `json:"to_version,omitempty"`
	Status      string `json:"status"`
}

// workspaceRollbackServiceFn rolls one service back to the given version,
// returning the version it came from and the version now serving.
type workspaceRollbackServiceFn func(svc *workspace.Service, info *app.AppInfo, target int, reason string) (from, to int, err error)

// runWorkspaceRollbackAll reverts every service of the workspace to its
// previous version in reverse dependency order: the services nothing depends
// on are taken down first, so a partially completed group rollback never
// leaves an upstream consumer running on top of an already reverted provider.
func runWorkspaceRollbackAll(rollbackOne workspaceRollbackServiceFn, reason string) error {
	return orchestrateWorkspaceRollback(rollbackOne, reason)
}

// orchestrateWorkspaceRollback is the injectable core of `phelix rollback
// --all`; tests pass a fake rollbackOne to exercise ordering and halting
// without touching the rollback engine.
func orchestrateWorkspaceRollback(rollbackOne workspaceRollbackServiceFn, reason string) error {
	ws, err := workspaceFromCWD()
	if err != nil {
		return err
	}
	forward, err := ws.TopoOrder()
	if err != nil {
		return err
	}
	order := reverseNames(forward)
	if err := authorizeWorkspaceServices(authz.ActionRollback, order); err != nil {
		return err
	}

	release, err := workspace.AcquireWorkspaceLock(ws.Name, "rollback")
	if err != nil {
		return err
	}
	defer release()

	state, err := loadWorkspaceState(ws)
	if err != nil {
		return err
	}

	printWorkspaceHeader(ws, "rolling back")
	restore := suppressEnvelopes()
	defer restore()

	startedAt := time.Now()
	views := make([]workspaceRollbackServiceView, 0, len(ws.Services))
	for _, name := range order {
		svc := ws.Service(name)
		info := findRegisteredApp(name)
		if info == nil {
			wsErr := phelixerr.Newf(
				phelixerr.CodeValidation,
				"service %q is not registered, so it has no version to roll back",
				name,
			)
			state.RecordServiceResult(name, "rollback", "failed", 0, phelixerr.Redact(wsErr.Error()))
			_ = workspace.SaveState(state)
			return wsErr
		}

		from, to, rollbackErr := rollbackOne(svc, info, 0, reason)
		if rollbackErr != nil {
			state.RecordServiceResult(name, "rollback", "failed", 0, phelixerr.Redact(rollbackErr.Error()))
			_ = workspace.SaveState(state)
			views = append(views, workspaceRollbackServiceView{Name: name, Status: "failed"})
			return phelixerr.Wrapf(phelixerr.CodeOf(rollbackErr), rollbackErr, "workspace rollback halted at service %q", name)
		}
		state.RecordServiceResult(name, "rollback", "succeeded", to, "")
		views = append(views, workspaceRollbackServiceView{Name: name, FromVersion: from, ToVersion: to, Status: "succeeded"})
		fmt.Printf("  %s %s %s -> %s\n",
			color.GreenString("✓"), color.CyanString(name),
			color.YellowString("v%d", from), color.YellowString("v%d", to))
	}
	if err := workspace.SaveState(state); err != nil {
		return err
	}

	fmt.Printf("%s Rollback complete for %s: %d service(s) in %s\n",
		color.GreenString("✓"), color.CyanString(ws.Name), len(views), time.Since(startedAt).Round(time.Millisecond))
	return writeEnvelopeResult(machine.Success("", workspaceRollbackResult{
		Workspace: ws.Name,
		Source:    ws.Source,
		Services:  views,
	}))
}

// workspaceRollbackService is the real workspace rollback step. It resolves
// the service's previous version and drives the same classic or zero-downtime
// rollback engine the single-app command uses, with no operation record of
// its own: the workspace run is one operation, not one per service.
func workspaceRollbackService(svc *workspace.Service, info *app.AppInfo, _ int, reason string) (int, int, error) {
	target, targetTag, targetSource, err := resolveRollbackTarget(info.Name, "")
	if err != nil {
		return 0, 0, err
	}
	from, err := deploy.CurrentVersion(info.Name)
	if err != nil {
		return 0, 0, phelixerr.Wrap(phelixerr.CodeConfiguration, "could not resolve current version", err)
	}

	state, err := classifyRollbackDeployState(info.Name)
	if err != nil {
		return from, 0, err
	}
	if state == nil {
		if err := rollbackClassic(info, info.Name, target, targetTag, targetSource, reason, 0, "", nil); err != nil {
			return from, target, err
		}
		return from, target, nil
	}
	if err := rollbackZeroDowntime(info, info.Name, target, targetTag, targetSource, state, reason, 0, "", nil); err != nil {
		return from, target, err
	}
	return from, target, nil
}

// reverseNames returns names in reverse order without mutating the input.
func reverseNames(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[len(names)-1-i] = name
	}
	return out
}
