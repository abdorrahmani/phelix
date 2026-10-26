package cmd

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/abdorrahmani/phelix/internal/app"
	"github.com/abdorrahmani/phelix/internal/builder"
	"github.com/abdorrahmani/phelix/internal/deploy"
	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/machine"
	phelixport "github.com/abdorrahmani/phelix/internal/port"
	"github.com/abdorrahmani/phelix/internal/project"
	"github.com/abdorrahmani/phelix/internal/workspace"
	"github.com/fatih/color"
)

var (
	// wsBuildAll switches `phelix build` into workspace orchestration mode.
	wsBuildAll bool
	// wsConcurrency bounds how many services of one dependency level build
	// at the same time.
	wsConcurrency int
	// wsPortBase is the first port auto-assigned to services without an
	// explicit port in the manifest.
	wsPortBase int
)

// wsPrintMu serializes failure summaries and result lines from parallel
// service builds so output stays readable.
var wsPrintMu sync.Mutex

// wsAppStateMu serializes every read and write of the process-global app
// registry made by parallel service builds. The registry is a plain map that
// LoadState replaces wholesale, so two goroutines touching it at once race
// even when they work on different services.
var wsAppStateMu sync.Mutex

// workspaceRegisterService installs or refreshes one service's app record
// under the registry lock and persists it, so parallel builds cannot
// interleave a map write with another service's save.
func workspaceRegisterService(svc *workspace.Service, lang builder.Language) (string, error) {
	wsAppStateMu.Lock()
	defer wsAppStateMu.Unlock()

	registered := findRegisteredApp(svc.Name)
	id := ""
	if registered != nil {
		id = registered.ID
		registered.Directory = svc.Path
		registered.Language = string(lang)
		registered.Port = svc.Port
		registered.BuildStatus = "building"
		registered.UpdatedAt = time.Now()
	} else {
		generated, err := app.Manager.GenerateAppID()
		if err != nil {
			return "", err
		}
		id = generated
		if err := createWorkspaceAppEntry(id, svc, lang); err != nil {
			return "", err
		}
	}
	if err := app.Manager.SaveState(); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to persist app state", err)
	}
	return id, nil
}

// workspaceApplyProjectState converges one service's phelix.yaml onto its app
// record and health config under the registry lock.
func workspaceApplyProjectState(projCfg *project.Config, svc *workspace.Service, id string) error {
	wsAppStateMu.Lock()
	defer wsAppStateMu.Unlock()

	if _, err := syncProjectResources(projCfg, id); err != nil {
		return err
	}
	if _, err := syncProjectWatching(projCfg, id); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "service %q: applying watching failed", svc.Name)
	}
	if err := syncProjectHealth(projCfg, id, svc.Name, svc.Port); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "service %q: applying health endpoints failed", svc.Name)
	}
	return nil
}

// workspaceMarkBuildFailed marks one service's build as failed under the
// registry lock, so a parallel build cannot race the map write.
func workspaceMarkBuildFailed(id string) {
	wsAppStateMu.Lock()
	defer wsAppStateMu.Unlock()

	markBuildFailed(id)
}

// workspaceMarkBuildSucceeded records a completed build on the app record.
func workspaceMarkBuildSucceeded(id string) {
	wsAppStateMu.Lock()
	defer wsAppStateMu.Unlock()

	appManager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return
	}
	if info, exists := appManager.Apps[id]; exists {
		info.BuildStatus = "success"
		info.UpdatedAt = time.Now()
		_ = appManager.SaveState()
	}
}

// workspaceBuildServiceFn is one workspace build step: compile the service
// and return its recorded version.
type workspaceBuildServiceFn func(svc *workspace.Service, extraArgs []string, buildMgr *builder.BuildManager) (int, error)

type workspaceBuildResult struct {
	Workspace string                      `json:"workspace"`
	Source    string                      `json:"source"`
	Services  []workspaceBuildServiceView `json:"services"`
}

type workspaceBuildServiceView struct {
	Name       string `json:"name"`
	Version    int    `json:"version,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"`
}

// runWorkspaceBuildAll compiles every service of the workspace in dependency
// order, parallelizing within a level. A failing service halts the run: no
// service of a later level starts, and the workspace state file records the
// outcome of every service that did run.
func runWorkspaceBuildAll(buildOne workspaceBuildServiceFn) error {
	return orchestrateWorkspaceBuild(buildOne, buildArgs)
}

// orchestrateWorkspaceBuild is the injectable core of `phelix build --all`;
// tests pass a fake buildOne to exercise ordering and halting without a
// compiler.
func orchestrateWorkspaceBuild(buildOne workspaceBuildServiceFn, extraArgs []string) error {
	ws, err := workspaceFromCWD()
	if err != nil {
		return err
	}
	if err := workspace.AssignPorts(ws, wsPortBase); err != nil {
		return err
	}
	levels, err := ws.TopoLevels()
	if err != nil {
		return err
	}

	release, err := workspace.AcquireWorkspaceLock(ws.Name, "build")
	if err != nil {
		return err
	}
	defer release()

	if err := loadAppStateForWorkspace(); err != nil {
		return phelixerr.Wrap(phelixerr.CodeFilesystem, "failed to load state", err)
	}
	state, err := loadWorkspaceState(ws)
	if err != nil {
		return err
	}

	buildMgr := builder.NewBuildManager()
	buildMgr.SetTimeout(buildTimeout)

	printWorkspaceHeader(ws, "building")
	startedAt := time.Now()
	outcomes := make(map[string]workspaceBuildServiceView, len(ws.Services))
	for _, level := range levels {
		levelErr := runParallelLevel(level, wsConcurrency, func(name string) error {
			svc := ws.Service(name)
			serviceStart := time.Now()
			version, buildErr := buildOne(svc, extraArgs, buildMgr)
			wsPrintMu.Lock()
			defer wsPrintMu.Unlock()
			if buildErr != nil {
				outcomes[name] = workspaceBuildServiceView{Name: name, DurationMS: time.Since(serviceStart).Milliseconds(), Status: "failed"}
				state.RecordServiceResult(name, "build", "failed", 0, phelixerr.Redact(buildErr.Error()))
				fmt.Printf("  %s %s failed: %v\n", color.RedString("✗"), color.CyanString(name), buildErr)
				return phelixerr.Wrapf(phelixerr.CodeOf(buildErr), buildErr, "workspace build halted at service %q", name)
			}
			outcomes[name] = workspaceBuildServiceView{Name: name, Version: version, DurationMS: time.Since(serviceStart).Milliseconds(), Status: "succeeded"}
			state.RecordServiceResult(name, "build", "succeeded", version, "")
			fmt.Printf("  %s %s %s (%s)\n", color.GreenString("✓"), color.CyanString(name), color.YellowString("v%d", version), time.Since(serviceStart).Round(time.Millisecond))
			return nil
		})
		if levelErr != nil {
			_ = workspace.SaveState(state)
			return levelErr
		}
	}
	if err := workspace.SaveState(state); err != nil {
		return err
	}

	fmt.Printf("%s Build complete for %s: %d service(s) in %s\n",
		color.GreenString("✓"), color.CyanString(ws.Name), len(ws.Services), time.Since(startedAt).Round(time.Millisecond))

	views := make([]workspaceBuildServiceView, 0, len(ws.Services))
	for _, svc := range ws.Services {
		views = append(views, outcomes[svc.Name])
	}
	return writeEnvelopeResult(machine.Success("", workspaceBuildResult{
		Workspace: ws.Name,
		Source:    ws.Source,
		Services:  views,
	}))
}

// buildWorkspaceService is the real workspace build step. It registers the
// service as a Phelix app on first build (directory and port come from the
// workspace, never from the process working directory), compiles it, and
// records the version with is_current=false per the two-phase promotion
// rule: only a deploy promotes.
func buildWorkspaceService(svc *workspace.Service, extraArgs []string, buildMgr *builder.BuildManager) (int, error) {
	if err := phelixport.Validate(svc.Port); err != nil {
		return 0, phelixerr.Wrapf(phelixerr.CodeConfiguration, err, "service %q has an invalid port", svc.Name)
	}

	projCfg, cfgErr := loadProjectConfigFrom(svc.Path)
	if cfgErr != nil {
		return 0, cfgErr
	}
	if projCfg != nil && projCfg.DeployRuntime() == project.RuntimeDocker {
		return 0, phelixerr.Newf(
			phelixerr.CodeUnsupportedProject,
			"service %q uses deploy.runtime: docker; workspace build --all only supports native services",
			svc.Name,
		)
	}

	lang := buildMgr.DetectLanguage(svc.Path)
	if !lang.IsSupported() {
		return 0, phelixerr.Newf(
			phelixerr.CodeUnsupportedProject,
			"service %q in %s is not a supported project (go.mod or Cargo.toml with a runnable entry required)",
			svc.Name, svc.Path,
		)
	}

	id, registerErr := workspaceRegisterService(svc, lang)
	if registerErr != nil {
		return 0, registerErr
	}
	if err := workspaceApplyProjectState(projCfg, svc, id); err != nil {
		return 0, err
	}

	release, err := deploy.AcquireDeployLock(svc.Name, "build")
	if err != nil {
		return 0, err
	}
	defer release()

	binPath := filepath.Join(svc.Path, fmt.Sprintf("app_%s", id))
	buildCtx, err := buildMgr.PrepareBuild(id, svc.Name, svc.Path, binPath, lang, extraArgs)
	if err != nil {
		workspaceMarkBuildFailed(id)
		wsPrintMu.Lock()
		printFailedBuildSummary(lang, nil)
		wsPrintMu.Unlock()
		return 0, phelixerr.Wrap(phelixerr.CodeBuildFailed, "build preparation failed", err)
	}
	if err := buildMgr.ExecuteBuild(buildCtx); err != nil {
		workspaceMarkBuildFailed(id)
		wsPrintMu.Lock()
		printFailedBuildSummary(lang, buildCtx.Config)
		wsPrintMu.Unlock()
		code := phelixerr.CodeOf(err)
		if code == phelixerr.CodeUnknown {
			code = phelixerr.CodeBuildFailed
		}
		return 0, phelixerr.Wrap(code, "build failed", err)
	}
	report := newNativeBuildReport(lang, buildCtx.Config, binPath)

	gitCommit := deploy.DetectGitCommit(svc.Path)
	rec, err := deploy.RecordFreshBuild(svc.Name, id, binPath, gitCommit, "", report, deploy.DefaultRetention{Max: 5}, &colorLogger{})
	if err != nil {
		return 0, phelixerr.Wrap(phelixerr.CodeBuildFailed, "could not record version", err)
	}

	workspaceMarkBuildSucceeded(id)
	return rec.Version, nil
}

// createWorkspaceAppEntry registers a fresh app whose directory is the
// service path, not the process working directory.
func createWorkspaceAppEntry(id string, svc *workspace.Service, lang builder.Language) error {
	appManager, ok := app.Manager.(*app.AppManager)
	if !ok {
		return phelixerr.New(phelixerr.CodeServer, "invalid app manager type")
	}
	now := time.Now()
	appManager.Apps[id] = &app.AppInfo{
		ID:          id,
		Name:        svc.Name,
		Status:      "initializing",
		BuildStatus: "building",
		CreatedAt:   now,
		UpdatedAt:   now,
		Directory:   svc.Path,
		Port:        svc.Port,
		Language:    string(lang),
	}
	return nil
}
